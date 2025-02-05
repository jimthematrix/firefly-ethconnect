// Copyright 2019 Kaleido

// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at

//     http://www.apache.org/licenses/LICENSE-2.0

// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package contracts

import (
	"bufio"
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/ioutil"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/go-openapi/spec"
	"github.com/julienschmidt/httprouter"
	"github.com/mholt/archiver"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"

	ethbinding "github.com/kaleido-io/ethbinding/pkg"
	"github.com/kaleido-io/ethconnect/internal/auth"
	ethconnecterrors "github.com/kaleido-io/ethconnect/internal/errors"
	"github.com/kaleido-io/ethconnect/internal/eth"
	"github.com/kaleido-io/ethconnect/internal/ethbind"
	"github.com/kaleido-io/ethconnect/internal/events"
	"github.com/kaleido-io/ethconnect/internal/messages"
	"github.com/kaleido-io/ethconnect/internal/openapi"
	"github.com/kaleido-io/ethconnect/internal/tx"
	"github.com/kaleido-io/ethconnect/internal/utils"
	"github.com/kaleido-io/ethconnect/internal/ws"
)

const (
	maxFormParsingMemory     = 32 << 20 // 32 MB
	errEventSupportMissing   = "Event support is not configured on this gateway"
	remoteRegistryContextKey = "isRemoteRegistry"
)

// SmartContractGateway provides gateway functions for OpenAPI 2.0 processing of Solidity contracts
type SmartContractGateway interface {
	PreDeploy(msg *messages.DeployContract) error
	PostDeploy(msg *messages.TransactionReceipt) error
	AddRoutes(router *httprouter.Router)
	SendReply(message interface{})
	Shutdown()
}

type smartContractGatewayInt interface {
	SmartContractGateway
	resolveContractAddr(registeredName string) (string, error)
	loadDeployMsgForInstance(addrHexNo0x string) (*messages.DeployContract, *contractInfo, error)
	loadDeployMsgByID(abi string) (*messages.DeployContract, *abiInfo, error)
	checkNameAvailable(name string, isRemote bool) error
}

// SmartContractGatewayConf configuration
type SmartContractGatewayConf struct {
	events.SubscriptionManagerConf
	StoragePath    string             `json:"storagePath"`
	BaseURL        string             `json:"baseURL"`
	RemoteRegistry RemoteRegistryConf `json:"registry,omitempty"` // JSON only config - no commandline
}

// CobraInitContractGateway standard naming for contract gateway command params
func CobraInitContractGateway(cmd *cobra.Command, conf *SmartContractGatewayConf) {
	cmd.Flags().StringVarP(&conf.StoragePath, "openapi-path", "I", "", "Path containing ABI + generated OpenAPI/Swagger 2.0 contact definitions")
	cmd.Flags().StringVarP(&conf.BaseURL, "openapi-baseurl", "U", "", "Base URL for generated OpenAPI/Swagger 2.0 contact definitions")
	events.CobraInitSubscriptionManager(cmd, &conf.SubscriptionManagerConf)
}

func (g *smartContractGW) withEventsAuth(handler httprouter.Handle) httprouter.Handle {
	return func(res http.ResponseWriter, req *http.Request, params httprouter.Params) {
		err := auth.AuthEventStreams(req.Context())
		if err != nil {
			log.Errorf("Unauthorized: %s", err)
			g.gatewayErrReply(res, req, ethconnecterrors.Errorf(ethconnecterrors.Unauthorized), 401)
			return
		}
		handler(res, req, params)
	}
}

func (g *smartContractGW) AddRoutes(router *httprouter.Router) {
	g.r2e.addRoutes(router)
	router.GET("/contracts", g.listContractsOrABIs)
	router.GET("/contracts/:address", g.getContractOrABI)
	router.POST("/abis", g.addABI)
	router.GET("/abis", g.listContractsOrABIs)
	router.GET("/abis/:abi", g.getContractOrABI)
	router.POST("/abis/:abi/:address", g.registerContract)
	router.GET("/instances/:instance_lookup", g.getRemoteRegistrySwaggerOrABI)
	router.GET("/i/:instance_lookup", g.getRemoteRegistrySwaggerOrABI)
	router.GET("/gateways/:gateway_lookup", g.getRemoteRegistrySwaggerOrABI)
	router.GET("/g/:gateway_lookup", g.getRemoteRegistrySwaggerOrABI)
	router.POST(events.StreamPathPrefix, g.withEventsAuth(g.createStream))
	router.PATCH(events.StreamPathPrefix+"/:id", g.withEventsAuth(g.updateStream))
	router.GET(events.StreamPathPrefix, g.withEventsAuth(g.listStreamsOrSubs))
	router.GET(events.SubPathPrefix, g.withEventsAuth(g.listStreamsOrSubs))
	router.GET(events.StreamPathPrefix+"/:id", g.withEventsAuth(g.getStreamOrSub))
	router.GET(events.SubPathPrefix+"/:id", g.withEventsAuth(g.getStreamOrSub))
	router.DELETE(events.StreamPathPrefix+"/:id", g.withEventsAuth(g.deleteStreamOrSub))
	router.DELETE(events.SubPathPrefix+"/:id", g.withEventsAuth(g.deleteStreamOrSub))
	router.POST(events.SubPathPrefix+"/:id/reset", g.withEventsAuth(g.resetSub))
	router.POST(events.StreamPathPrefix+"/:id/suspend", g.withEventsAuth(g.suspendOrResumeStream))
	router.POST(events.StreamPathPrefix+"/:id/resume", g.withEventsAuth(g.suspendOrResumeStream))
}

func (g *smartContractGW) SendReply(message interface{}) {
	g.ws.SendReply(message)
}

// NewSmartContractGateway constructor
func NewSmartContractGateway(conf *SmartContractGatewayConf, txnConf *tx.TxnProcessorConf, rpc eth.RPCClient, processor tx.TxnProcessor, asyncDispatcher REST2EthAsyncDispatcher, ws ws.WebSocketChannels) (SmartContractGateway, error) {
	var baseURL *url.URL
	var err error
	if conf.BaseURL != "" {
		if baseURL, err = url.Parse(conf.BaseURL); err != nil {
			log.Warnf("Unable to parse smart contract gateway base URL '%s': %s", conf.BaseURL, err)
		}
	}
	if baseURL == nil {
		baseURL, _ = url.Parse("http://localhost:8080")
	}
	log.Infof("OpenAPI Smart Contract Gateway configured with base URL '%s'", baseURL.String())
	gw := &smartContractGW{
		conf:                  conf,
		rr:                    NewRemoteRegistry(&conf.RemoteRegistry),
		contractIndex:         make(map[string]messages.TimeSortable),
		contractRegistrations: make(map[string]*contractInfo),
		abiIndex:              make(map[string]messages.TimeSortable),
		baseSwaggerConf: &openapi.ABI2SwaggerConf{
			ExternalHost:     baseURL.Host,
			ExternalRootPath: baseURL.Path,
			ExternalSchemes:  []string{baseURL.Scheme},
			OrionPrivateAPI:  txnConf.OrionPrivateAPIS,
			BasicAuth:        true,
		},
		ws: ws,
	}
	if err = gw.rr.init(); err != nil {
		return nil, err
	}
	syncDispatcher := newSyncDispatcher(processor)
	if conf.EventLevelDBPath != "" {
		gw.sm = events.NewSubscriptionManager(&conf.SubscriptionManagerConf, rpc, gw.ws)
		err = gw.sm.Init()
		if err != nil {
			return nil, ethconnecterrors.Errorf(ethconnecterrors.RESTGatewayEventManagerInitFailed, err)
		}
	}
	gw.r2e = newREST2eth(gw, rpc, gw.sm, gw.rr, processor, asyncDispatcher, syncDispatcher)
	gw.buildIndex()
	return gw, nil
}

type smartContractGW struct {
	conf                  *SmartContractGatewayConf
	sm                    events.SubscriptionManager
	rr                    RemoteRegistry
	r2e                   *rest2eth
	ws                    ws.WebSocketChannels
	contractIndex         map[string]messages.TimeSortable
	contractRegistrations map[string]*contractInfo
	idxLock               sync.Mutex
	abiIndex              map[string]messages.TimeSortable
	baseSwaggerConf       *openapi.ABI2SwaggerConf
}

// contractInfo is the minimal data structure we keep in memory, indexed by address
// ONLY used for local registry. Remote registry handles its own storage/caching
type contractInfo struct {
	messages.TimeSorted
	Address      string `json:"address"`
	Path         string `json:"path"`
	ABI          string `json:"abi"`
	SwaggerURL   string `json:"openapi"`
	RegisteredAs string `json:"registeredAs"`
}

// abiInfo is the minimal data structure we keep in memory, indexed by our own UUID
type abiInfo struct {
	messages.TimeSorted
	ID              string `json:"id"`
	Name            string `json:"name"`
	Description     string `json:"description"`
	Path            string `json:"path"`
	Deployable      bool   `json:"deployable"`
	SwaggerURL      string `json:"openapi"`
	CompilerVersion string `json:"compilerVersion"`
}

// remoteContractInfo is the ABI raw data back out of the REST API gateway with bytecode
type remoteContractInfo struct {
	ID      string                   `json:"id"`
	Address string                   `json:"address,omitempty"`
	ABI     ethbinding.ABIMarshaling `json:"abi"`
}

func (i *contractInfo) GetID() string {
	return i.Address
}

func (i *abiInfo) GetID() string {
	return i.ID
}

func (g *smartContractGW) storeNewContractInfo(addrHexNo0x, abiID, pathName, registerAs string) (*contractInfo, error) {
	contractInfo := &contractInfo{
		Address:      addrHexNo0x,
		ABI:          abiID,
		Path:         "/contracts/" + pathName,
		SwaggerURL:   g.conf.BaseURL + "/contracts/" + pathName + "?swagger",
		RegisteredAs: registerAs,
		TimeSorted: messages.TimeSorted{
			CreatedISO8601: time.Now().UTC().Format(time.RFC3339),
		},
	}
	if err := g.storeContractInfo(contractInfo); err != nil {
		return nil, err
	}
	return contractInfo, nil
}

func isRemote(msg messages.CommonHeaders) bool {
	ctxMap := msg.Context
	if isRemoteGeneric, ok := ctxMap[remoteRegistryContextKey]; ok {
		if isRemote, ok := isRemoteGeneric.(bool); ok {
			return isRemote
		}
	}
	return false
}

// PostDeploy callback processes the transaction receipt and generates the Swagger
func (g *smartContractGW) PostDeploy(msg *messages.TransactionReceipt) error {

	requestID := msg.Headers.ReqID

	// We use the ethereum address of the contract, without the 0x prefix, and
	// all in lower case, as the name of the file and the path root of the Swagger operations
	if msg.ContractAddress == nil {
		return ethconnecterrors.Errorf(ethconnecterrors.RESTGatewayPostDeployMissingAddress, requestID)
	}
	addrHexNo0x := strings.ToLower(msg.ContractAddress.Hex()[2:])

	// Generate and store the swagger
	basePath := "/contracts/"
	isRemote := isRemote(msg.Headers.CommonHeaders)
	if isRemote {
		basePath = "/instances/"
	}
	registeredName := msg.RegisterAs
	if registeredName == "" {
		registeredName = addrHexNo0x
	}

	if msg.Headers.MsgType == messages.MsgTypeTransactionSuccess {
		msg.ContractSwagger = g.conf.BaseURL + basePath + registeredName + "?openapi"
		msg.ContractUI = g.conf.BaseURL + basePath + registeredName + "?ui"

		var err error
		if isRemote {
			if msg.RegisterAs != "" {
				err = g.rr.registerInstance(msg.RegisterAs, "0x"+addrHexNo0x)
			}
		} else {
			_, err = g.storeNewContractInfo(addrHexNo0x, requestID, registeredName, msg.RegisterAs)
		}
		return err
	}
	return nil
}

func (g *smartContractGW) swaggerForRemoteRegistry(swaggerGen *openapi.ABI2Swagger, apiName, addr string, factoryOnly bool, abi *ethbinding.RuntimeABI, devdoc, path string) *spec.Swagger {
	var swagger *spec.Swagger
	if addr == "" {
		swagger = swaggerGen.Gen4Factory(path, apiName, factoryOnly, true, &abi.ABI, devdoc)
	} else {
		swagger = swaggerGen.Gen4Instance(path, apiName, &abi.ABI, devdoc)
	}
	return swagger
}

func (g *smartContractGW) swaggerForABI(swaggerGen *openapi.ABI2Swagger, abiID, apiName string, factoryOnly bool, abi *ethbinding.RuntimeABI, devdoc string, addrHexNo0x, registerAs string) *spec.Swagger {
	// Ensure we have a contract name in all cases, as the Swagger
	// won't be valid without a title
	if apiName == "" {
		apiName = abiID
	}
	var swagger *spec.Swagger
	if addrHexNo0x != "" {
		pathSuffix := url.QueryEscape(registerAs)
		if pathSuffix == "" {
			pathSuffix = addrHexNo0x
		}
		swagger = swaggerGen.Gen4Instance("/contracts/"+pathSuffix, apiName, &abi.ABI, devdoc)
		if registerAs != "" {
			swagger.Info.AddExtension("x-firefly-registered-name", pathSuffix)
		}
	} else {
		swagger = swaggerGen.Gen4Factory("/abis/"+abiID, apiName, factoryOnly, false, &abi.ABI, devdoc)
	}

	// Add in an extension to the Swagger that points back at the filename of the deployment info
	if abiID != "" {
		swagger.Info.AddExtension("x-firefly-deployment-id", abiID)
	}

	return swagger
}

func (g *smartContractGW) storeContractInfo(info *contractInfo) error {
	if err := g.addToContractIndex(info); err != nil {
		return err
	}
	infoFile := path.Join(g.conf.StoragePath, "contract_"+info.Address+".instance.json")
	instanceBytes, _ := json.MarshalIndent(info, "", "  ")
	log.Infof("%s: Storing contract instance JSON to '%s'", info.ABI, infoFile)
	if err := ioutil.WriteFile(infoFile, instanceBytes, 0664); err != nil {
		return ethconnecterrors.Errorf(ethconnecterrors.RESTGatewayLocalStoreContractSave, err)
	}
	return nil
}

func (g *smartContractGW) resolveContractAddr(registeredName string) (string, error) {
	nameUnescaped, _ := url.QueryUnescape(registeredName)
	info, exists := g.contractRegistrations[nameUnescaped]
	if !exists {
		return "", ethconnecterrors.Errorf(ethconnecterrors.RESTGatewayLocalStoreContractLoad, registeredName)
	}
	log.Infof("%s -> 0x%s", registeredName, info.Address)
	return info.Address, nil
}

func (g *smartContractGW) loadDeployMsgForInstance(addrHex string) (*messages.DeployContract, *contractInfo, error) {
	addrHexNo0x := strings.TrimPrefix(strings.ToLower(addrHex), "0x")
	info, exists := g.contractIndex[addrHexNo0x]
	if !exists {
		return nil, nil, ethconnecterrors.Errorf(ethconnecterrors.RESTGatewayLocalStoreContractNotFound, addrHexNo0x)
	}
	deployMsg, _, err := g.loadDeployMsgByID(info.(*contractInfo).ABI)
	return deployMsg, info.(*contractInfo), err
}

func (g *smartContractGW) loadDeployMsgByID(id string) (*messages.DeployContract, *abiInfo, error) {
	var info *abiInfo
	var msg *messages.DeployContract
	ts, exists := g.abiIndex[id]
	if !exists {
		log.Infof("ABI with ID %s not found locally", id)
		return nil, nil, ethconnecterrors.Errorf(ethconnecterrors.RESTGatewayLocalStoreABINotFound, id)
	}
	deployFile := path.Join(g.conf.StoragePath, "abi_"+id+".deploy.json")
	deployBytes, err := ioutil.ReadFile(deployFile)
	if err != nil {
		return nil, nil, ethconnecterrors.Errorf(ethconnecterrors.RESTGatewayLocalStoreABILoad, id, err)
	}
	msg = &messages.DeployContract{}
	if err = json.Unmarshal(deployBytes, msg); err != nil {
		return nil, nil, ethconnecterrors.Errorf(ethconnecterrors.RESTGatewayLocalStoreABIParse, id, err)
	}
	info = ts.(*abiInfo)
	return msg, info, nil
}

// PreDeploy
// - compiles the Solidity (if not precomplied),
// - puts the code into the message to avoid a recompile later
// - stores the ABI under the MsgID (can later be bound to an address)
// *** caller is responsible for ensuring unique Header.ID ***
func (g *smartContractGW) PreDeploy(msg *messages.DeployContract) (err error) {
	solidity := msg.Solidity
	var compiled *eth.CompiledSolidity
	if solidity != "" {
		if compiled, err = eth.CompileContract(solidity, msg.ContractName, msg.CompilerVersion, msg.EVMVersion); err != nil {
			return err
		}
	}
	if !isRemote(msg.Headers.CommonHeaders) {
		_, err = g.storeDeployableABI(msg, compiled)
	}
	return err
}

func (g *smartContractGW) storeDeployableABI(msg *messages.DeployContract, compiled *eth.CompiledSolidity) (*abiInfo, error) {

	if compiled != nil {
		msg.Compiled = compiled.Compiled
		msg.ABI = compiled.ABI
		msg.DevDoc = compiled.DevDoc
		msg.ContractName = compiled.ContractName
		msg.CompilerVersion = compiled.ContractInfo.CompilerVersion
	} else if msg.ABI == nil {
		return nil, ethconnecterrors.Errorf(ethconnecterrors.RESTGatewayLocalStoreMissingABI)
	}

	runtimeABI, err := ethbind.API.ABIMarshalingToABIRuntime(msg.ABI)
	if err != nil {
		return nil, ethconnecterrors.Errorf(ethconnecterrors.RESTGatewayInvalidABI, err)
	}

	requestID := msg.Headers.ID
	// We store the swagger in a generic format that can be used to deploy
	// additional instances, or generically call other instances
	// Generate and store the swagger
	swagger := g.swaggerForABI(openapi.NewABI2Swagger(g.baseSwaggerConf), requestID, msg.ContractName, false, runtimeABI, msg.DevDoc, "", "")
	msg.Description = swagger.Info.Description // Swagger generation parses the devdoc
	info := g.addToABIIndex(requestID, msg, time.Now().UTC())

	g.writeAbiInfo(requestID, msg)

	// We remove the solidity payload from the message, as we've consumed
	// it by compiling and there is no need to serialize it again.
	// The messages should contain compiled bytes at this
	msg.Solidity = ""

	return info, nil

}

func (g *smartContractGW) gatewayErrReply(res http.ResponseWriter, req *http.Request, err error, status int) {
	log.Errorf("<-- %s %s [%d]: %s", req.Method, req.URL, status, err)
	reply, _ := json.Marshal(&restErrMsg{Message: err.Error()})
	res.Header().Set("Content-Type", "application/json")
	res.WriteHeader(status)
	res.Write(reply)
	return
}

func (g *smartContractGW) writeAbiInfo(requestID string, msg *messages.DeployContract) error {
	// We store all the details from our compile, or the user-supplied
	// details, in a file under the message ID.
	infoFile := path.Join(g.conf.StoragePath, "abi_"+requestID+".deploy.json")
	infoBytes, _ := json.MarshalIndent(msg, "", "  ")
	log.Infof("%s: Stashing deployment details to '%s'", requestID, infoFile)
	if err := ioutil.WriteFile(infoFile, infoBytes, 0664); err != nil {
		return ethconnecterrors.Errorf(ethconnecterrors.RESTGatewayLocalStoreContractSavePostDeploy, requestID, err)
	}
	return nil
}

func (g *smartContractGW) buildIndex() {
	log.Infof("Building installed smart contract index")
	legacyContractMatcher, _ := regexp.Compile("^contract_([0-9a-z]{40})\\.swagger\\.json$")
	instanceMatcher, _ := regexp.Compile("^contract_([0-9a-z]{40})\\.instance\\.json$")
	abiMatcher, _ := regexp.Compile("^abi_([0-9a-z-]+)\\.deploy.json$")
	files, err := ioutil.ReadDir(g.conf.StoragePath)
	if err != nil {
		log.Errorf("Failed to read directory %s: %s", g.conf.StoragePath, err)
		return
	}
	for _, file := range files {
		fileName := file.Name()
		legacyContractGroups := legacyContractMatcher.FindStringSubmatch(fileName)
		abiGroups := abiMatcher.FindStringSubmatch(fileName)
		instanceGroups := instanceMatcher.FindStringSubmatch(fileName)
		if legacyContractGroups != nil {
			g.migrateLegacyContract(legacyContractGroups[1], path.Join(g.conf.StoragePath, fileName), file.ModTime())
		} else if instanceGroups != nil {
			g.addFileToContractIndex(instanceGroups[1], path.Join(g.conf.StoragePath, fileName))
		} else if abiGroups != nil {
			g.addFileToABIIndex(abiGroups[1], path.Join(g.conf.StoragePath, fileName), file.ModTime())
		}
	}
	log.Infof("Smart contract index built. %d entries", len(g.contractIndex))
}

func (g *smartContractGW) migrateLegacyContract(address, fileName string, createdTime time.Time) {
	swaggerFile, err := os.OpenFile(fileName, os.O_RDONLY, 0)
	if err != nil {
		log.Errorf("Failed to load Swagger file %s: %s", fileName, err)
		return
	}
	defer swaggerFile.Close()
	var swagger spec.Swagger
	err = json.NewDecoder(bufio.NewReader(swaggerFile)).Decode(&swagger)
	if err != nil {
		log.Errorf("Failed to parse Swagger file %s: %s", fileName, err)
		return
	}
	if swagger.Info == nil {
		log.Errorf("Failed to migrate invalid Swagger file %s", fileName)
		return
	}
	var registeredAs string
	if ext, exists := swagger.Info.Extensions["x-firefly-registered-name"]; exists {
		registeredAs = ext.(string)
	}
	if ext, exists := swagger.Info.Extensions["x-firefly-deployment-id"]; exists {
		_, err := g.storeNewContractInfo(address, ext.(string), address, registeredAs)
		if err != nil {
			log.Errorf("Failed to write migrated instance file: %s", err)
			return
		}

		if err := os.Remove(fileName); err != nil {
			log.Errorf("Failed to clean-up migrated file %s: %s", fileName, err)
		}

	} else {
		log.Warnf("Swagger cannot be migrated due to missing 'x-firefly-deployment-id' extension: %s", fileName)
	}

}

func (g *smartContractGW) addFileToContractIndex(address, fileName string) {
	contractFile, err := os.OpenFile(fileName, os.O_RDONLY, 0)
	if err != nil {
		log.Errorf("Failed to load contract instance file %s: %s", fileName, err)
		return
	}
	defer contractFile.Close()
	var contractInfo contractInfo
	err = json.NewDecoder(bufio.NewReader(contractFile)).Decode(&contractInfo)
	if err != nil {
		log.Errorf("Failed to parse contract instnace deployment file %s: %s", fileName, err)
		return
	}
	g.addToContractIndex(&contractInfo)
}

func (g *smartContractGW) addFileToABIIndex(id, fileName string, createdTime time.Time) {
	deployFile, err := os.OpenFile(fileName, os.O_RDONLY, 0)
	if err != nil {
		log.Errorf("Failed to load ABI deployment file %s: %s", fileName, err)
		return
	}
	defer deployFile.Close()
	var deployMsg messages.DeployContract
	err = json.NewDecoder(bufio.NewReader(deployFile)).Decode(&deployMsg)
	if err != nil {
		log.Errorf("Failed to parse ABI deployment file %s: %s", fileName, err)
		return
	}
	g.addToABIIndex(id, &deployMsg, createdTime)
}

func (g *smartContractGW) checkNameAvailable(registerAs string, isRemote bool) error {
	if isRemote {
		msg, err := g.rr.loadFactoryForInstance(registerAs, false)
		if err != nil {
			return err
		} else if msg != nil {
			return ethconnecterrors.Errorf(ethconnecterrors.RESTGatewayFriendlyNameClash, msg.Address, registerAs)
		}
		return nil
	}
	if existing, exists := g.contractRegistrations[registerAs]; exists {
		return ethconnecterrors.Errorf(ethconnecterrors.RESTGatewayFriendlyNameClash, existing.Address, registerAs)
	}
	return nil
}

func (g *smartContractGW) addToContractIndex(info *contractInfo) error {
	g.idxLock.Lock()
	defer g.idxLock.Unlock()
	if info.RegisteredAs != "" {
		// Protect against overwrite
		if err := g.checkNameAvailable(info.RegisteredAs, false); err != nil {
			return err
		}
		log.Infof("Registering %s as '%s'", info.Address, info.RegisteredAs)
		g.contractRegistrations[info.RegisteredAs] = info
	}
	g.contractIndex[info.Address] = info
	return nil
}

func (g *smartContractGW) addToABIIndex(id string, deployMsg *messages.DeployContract, createdTime time.Time) *abiInfo {
	g.idxLock.Lock()
	info := &abiInfo{
		ID:              id,
		Name:            deployMsg.ContractName,
		Description:     deployMsg.Description,
		Deployable:      len(deployMsg.Compiled) > 0,
		CompilerVersion: deployMsg.CompilerVersion,
		Path:            "/abis/" + id,
		SwaggerURL:      g.conf.BaseURL + "/abis/" + id + "?swagger",
		TimeSorted: messages.TimeSorted{
			CreatedISO8601: createdTime.UTC().Format(time.RFC3339),
		},
	}
	g.abiIndex[id] = info
	g.idxLock.Unlock()
	return info
}

// listContracts sorts by Title then Address and returns an array
func (g *smartContractGW) listContractsOrABIs(res http.ResponseWriter, req *http.Request, params httprouter.Params) {
	log.Infof("--> %s %s", req.Method, req.URL)

	var index map[string]messages.TimeSortable
	if strings.HasSuffix(req.URL.Path, "contracts") {
		index = g.contractIndex
	} else {
		index = g.abiIndex
	}

	// Get an array copy of the current list
	g.idxLock.Lock()
	retval := make([]messages.TimeSortable, 0, len(index))
	for _, info := range index {
		retval = append(retval, info)
	}
	g.idxLock.Unlock()

	// Do the sort by Title then Address
	sort.Slice(retval, func(i, j int) bool {
		return retval[i].IsLessThan(retval[i], retval[j])
	})

	status := 200
	log.Infof("<-- %s %s [%d]", req.Method, req.URL, status)
	res.Header().Set("Content-Type", "application/json")
	res.WriteHeader(status)
	enc := json.NewEncoder(res)
	enc.SetIndent("", "  ")
	enc.Encode(&retval)
}

// createStream creates a stream
func (g *smartContractGW) createStream(res http.ResponseWriter, req *http.Request, params httprouter.Params) {
	log.Infof("--> %s %s", req.Method, req.URL)

	if g.sm == nil {
		g.gatewayErrReply(res, req, errors.New(errEventSupportMissing), 405)
		return
	}

	var spec events.StreamInfo
	if err := json.NewDecoder(req.Body).Decode(&spec); err != nil {
		g.gatewayErrReply(res, req, ethconnecterrors.Errorf(ethconnecterrors.RESTGatewayEventStreamInvalid, err), 400)
		return
	}

	newSpec, err := g.sm.AddStream(req.Context(), &spec)
	if err != nil {
		g.gatewayErrReply(res, req, err, 400)
		return
	}

	status := 200
	log.Infof("<-- %s %s [%d]", req.Method, req.URL, status)
	res.Header().Set("Content-Type", "application/json")
	res.WriteHeader(status)
	enc := json.NewEncoder(res)
	enc.SetIndent("", "  ")
	enc.Encode(&newSpec)
}

// updateStream updates a stream
func (g *smartContractGW) updateStream(res http.ResponseWriter, req *http.Request, params httprouter.Params) {
	log.Infof("--> %s %s", req.Method, req.URL)

	if g.sm == nil {
		g.gatewayErrReply(res, req, errors.New(errEventSupportMissing), 405)
		return
	}

	streamID := params.ByName("id")
	_, err := g.sm.StreamByID(req.Context(), streamID)
	if err != nil {
		g.gatewayErrReply(res, req, err, 404)
		return
	}
	var spec events.StreamInfo
	if err := json.NewDecoder(req.Body).Decode(&spec); err != nil {
		g.gatewayErrReply(res, req, ethconnecterrors.Errorf(ethconnecterrors.RESTGatewayEventStreamInvalid, err), 400)
		return
	}
	newSpec, err := g.sm.UpdateStream(req.Context(), streamID, &spec)
	if err != nil {
		g.gatewayErrReply(res, req, err, 500)
		return
	}

	status := 200
	log.Infof("<-- %s %s [%d]", req.Method, req.URL, status)
	res.Header().Set("Content-Type", "application/json")
	res.WriteHeader(status)
	enc := json.NewEncoder(res)
	enc.SetIndent("", "  ")
	enc.Encode(&newSpec)
}

// listStreamsOrSubs sorts by Title then Address and returns an array
func (g *smartContractGW) listStreamsOrSubs(res http.ResponseWriter, req *http.Request, params httprouter.Params) {
	log.Infof("--> %s %s", req.Method, req.URL)

	if g.sm == nil {
		g.gatewayErrReply(res, req, errors.New(errEventSupportMissing), 405)
		return
	}

	var results []messages.TimeSortable
	if strings.HasPrefix(req.URL.Path, events.SubPathPrefix) {
		subs := g.sm.Subscriptions(req.Context())
		results = make([]messages.TimeSortable, len(subs))
		for i := range subs {
			results[i] = subs[i]
		}
	} else {
		streams := g.sm.Streams(req.Context())
		results = make([]messages.TimeSortable, len(streams))
		for i := range streams {
			results[i] = streams[i]
		}
	}

	// Do the sort
	sort.Slice(results, func(i, j int) bool {
		return results[i].IsLessThan(results[i], results[j])
	})

	status := 200
	log.Infof("<-- %s %s [%d]", req.Method, req.URL, status)
	res.Header().Set("Content-Type", "application/json")
	res.WriteHeader(status)
	enc := json.NewEncoder(res)
	enc.SetIndent("", "  ")
	enc.Encode(&results)
}

// getStreamOrSub returns stream over REST
func (g *smartContractGW) getStreamOrSub(res http.ResponseWriter, req *http.Request, params httprouter.Params) {
	log.Infof("--> %s %s", req.Method, req.URL)

	if g.sm == nil {
		g.gatewayErrReply(res, req, errors.New(errEventSupportMissing), 405)
		return
	}

	var retval interface{}
	var err error
	if strings.HasPrefix(req.URL.Path, events.SubPathPrefix) {
		retval, err = g.sm.SubscriptionByID(req.Context(), params.ByName("id"))
	} else {
		retval, err = g.sm.StreamByID(req.Context(), params.ByName("id"))
	}
	if err != nil {
		g.gatewayErrReply(res, req, err, 404)
		return
	}

	status := 200
	log.Infof("<-- %s %s [%d]", req.Method, req.URL, status)
	res.Header().Set("Content-Type", "application/json")
	res.WriteHeader(status)
	enc := json.NewEncoder(res)
	enc.SetIndent("", "  ")
	enc.Encode(retval)
}

// deleteStreamOrSub deletes stream over REST
func (g *smartContractGW) deleteStreamOrSub(res http.ResponseWriter, req *http.Request, params httprouter.Params) {
	log.Infof("--> %s %s", req.Method, req.URL)

	if g.sm == nil {
		g.gatewayErrReply(res, req, errors.New(errEventSupportMissing), 405)
		return
	}

	var err error
	if strings.HasPrefix(req.URL.Path, events.SubPathPrefix) {
		err = g.sm.DeleteSubscription(req.Context(), params.ByName("id"))
	} else {
		err = g.sm.DeleteStream(req.Context(), params.ByName("id"))
	}
	if err != nil {
		g.gatewayErrReply(res, req, err, 500)
		return
	}

	status := 204
	log.Infof("<-- %s %s [%d]", req.Method, req.URL, status)
	res.Header().Set("Content-Type", "application/json")
	res.WriteHeader(status)
}

// resetSub resets subscription over REST
func (g *smartContractGW) resetSub(res http.ResponseWriter, req *http.Request, params httprouter.Params) {
	log.Infof("--> %s %s", req.Method, req.URL)

	if g.sm == nil {
		g.gatewayErrReply(res, req, errors.New(errEventSupportMissing), 405)
		return
	}

	var body struct {
		FromBlock string `json:"fromBlock"`
	}
	err := json.NewDecoder(req.Body).Decode(&body)
	if err == nil {
		err = g.sm.ResetSubscription(req.Context(), params.ByName("id"), body.FromBlock)
	}
	if err != nil {
		g.gatewayErrReply(res, req, err, 500)
		return
	}

	status := 204
	log.Infof("<-- %s %s [%d]", req.Method, req.URL, status)
	res.Header().Set("Content-Type", "application/json")
	res.WriteHeader(status)
}

// suspendOrResumeStream suspends or resumes a stream
func (g *smartContractGW) suspendOrResumeStream(res http.ResponseWriter, req *http.Request, params httprouter.Params) {
	log.Infof("--> %s %s", req.Method, req.URL)

	if g.sm == nil {
		g.gatewayErrReply(res, req, errors.New(errEventSupportMissing), 405)
		return
	}

	var err error
	if strings.HasSuffix(req.URL.Path, "resume") {
		err = g.sm.ResumeStream(req.Context(), params.ByName("id"))
	} else {
		err = g.sm.SuspendStream(req.Context(), params.ByName("id"))
	}
	if err != nil {
		g.gatewayErrReply(res, req, err, 500)
		return
	}

	status := 204
	log.Infof("<-- %s %s [%d]", req.Method, req.URL, status)
	res.Header().Set("Content-Type", "application/json")
	res.WriteHeader(status)
}

func (g *smartContractGW) resolveAddressOrName(id string) (deployMsg *messages.DeployContract, registeredName string, info *contractInfo, err error) {
	deployMsg, info, err = g.loadDeployMsgForInstance(id)
	if err != nil {
		var origErr = err
		registeredName = id
		if id, err = g.resolveContractAddr(registeredName); err != nil {
			log.Infof("%s is not a friendly name: %s", registeredName, err)
			return nil, "", nil, origErr
		}
		if deployMsg, info, err = g.loadDeployMsgForInstance(id); err != nil {
			return nil, "", nil, err
		}
	}
	return deployMsg, registeredName, info, err
}

func (g *smartContractGW) isSwaggerRequest(req *http.Request) (swaggerGen *openapi.ABI2Swagger, uiRequest, factoryOnly, abiRequest, refreshABI bool, from string) {
	req.ParseForm()
	var swaggerRequest bool
	if vs := req.Form["swagger"]; len(vs) > 0 {
		swaggerRequest = strings.ToLower(vs[0]) != "false"
	}
	if vs := req.Form["openapi"]; len(vs) > 0 {
		swaggerRequest = strings.ToLower(vs[0]) != "false"
	}
	if vs := req.Form["ui"]; len(vs) > 0 {
		uiRequest = strings.ToLower(vs[0]) != "false"
	}
	if vs := req.Form["factory"]; len(vs) > 0 {
		factoryOnly = strings.ToLower(vs[0]) != "false"
	}
	if vs := req.Form["abi"]; len(vs) > 0 {
		abiRequest = strings.ToLower(vs[0]) != "false"
	}
	if vs := req.Form["refresh"]; len(vs) > 0 {
		refreshABI = strings.ToLower(vs[0]) != "false"
	}
	from = req.FormValue("from")
	if swaggerRequest {
		var conf = *g.baseSwaggerConf
		if vs := req.Form["noauth"]; len(vs) > 0 {
			conf.BasicAuth = strings.ToLower(vs[0]) == "false"
		}
		if vs := req.Form["schemes"]; len(vs) > 0 {
			requested := strings.Split(vs[0], ",")
			conf.ExternalSchemes = []string{}
			for _, scheme := range requested {
				// Only allow http and https
				if scheme == "http" || scheme == "https" {
					conf.ExternalSchemes = append(conf.ExternalSchemes, scheme)
				} else {
					log.Warnf("Excluded unknown scheme: %s", scheme)
				}
			}
		}
		swaggerGen = openapi.NewABI2Swagger(&conf)
	}
	return
}

func (g *smartContractGW) replyWithSwagger(res http.ResponseWriter, req *http.Request, swagger *spec.Swagger, id, from string) {
	if from != "" {
		if swagger.Parameters != nil {
			if param, exists := swagger.Parameters["fromParam"]; exists {
				param.SimpleSchema.Default = from
				swagger.Parameters["fromParam"] = param
			}
		}
	}
	swaggerBytes, _ := json.MarshalIndent(&swagger, "", "  ")

	log.Infof("<-- %s %s [%d]", req.Method, req.URL, 200)
	res.Header().Set("Content-Type", "application/json")
	if vs := req.Form["download"]; len(vs) > 0 {
		res.Header().Set("Content-Disposition", "attachment; filename=\""+id+".swagger.json\"")
	}
	res.WriteHeader(200)
	res.Write(swaggerBytes)
}

func (g *smartContractGW) getContractOrABI(res http.ResponseWriter, req *http.Request, params httprouter.Params) {
	log.Infof("--> %s %s", req.Method, req.URL)
	swaggerGen, uiRequest, factoryOnly, abiRequest, _, from := g.isSwaggerRequest(req)
	id := strings.TrimPrefix(strings.ToLower(params.ByName("address")), "0x")
	prefix := "contract"
	if id == "" {
		id = strings.ToLower(params.ByName("abi"))
		prefix = "abi"
	}
	// For safety we always check our sanitized address index in memory, before checking the filesystem
	var registeredName string
	var err error
	var deployMsg *messages.DeployContract
	var info messages.TimeSortable
	var abiID string
	if prefix == "contract" {
		if deployMsg, registeredName, info, err = g.resolveAddressOrName(params.ByName("address")); err != nil {
			g.gatewayErrReply(res, req, err, 404)
			return
		}
	} else {
		abiID = id
		deployMsg, info, err = g.loadDeployMsgByID(abiID)
		if err != nil {
			g.gatewayErrReply(res, req, err, 404)
			return
		}
	}
	if uiRequest {
		g.writeHTMLForUI(prefix, id, from, (prefix == "abi"), factoryOnly, res)
	} else if swaggerGen != nil {
		addr := params.ByName("address")
		runtimeABI, err := ethbind.API.ABIMarshalingToABIRuntime(deployMsg.ABI)
		if err != nil {
			g.gatewayErrReply(res, req, ethconnecterrors.Errorf(ethconnecterrors.RESTGatewayInvalidABI, err), 404)
			return
		}
		swagger := g.swaggerForABI(swaggerGen, abiID, deployMsg.ContractName, factoryOnly, runtimeABI, deployMsg.DevDoc, addr, registeredName)
		g.replyWithSwagger(res, req, swagger, id, from)
	} else if abiRequest {
		log.Infof("<-- %s %s [%d]", req.Method, req.URL, 200)
		res.Header().Set("Content-Type", "application/json")
		res.WriteHeader(200)
		enc := json.NewEncoder(res)
		enc.SetIndent("", "  ")
		enc.Encode(deployMsg.ABI)
	} else {
		log.Infof("<-- %s %s [%d]", req.Method, req.URL, 200)
		res.Header().Set("Content-Type", "application/json")
		res.WriteHeader(200)
		enc := json.NewEncoder(res)
		enc.SetIndent("", "  ")
		enc.Encode(info)
	}
}

func (g *smartContractGW) getRemoteRegistrySwaggerOrABI(res http.ResponseWriter, req *http.Request, params httprouter.Params) {
	log.Infof("--> %s %s", req.Method, req.URL)

	swaggerGen, uiRequest, factoryOnly, abiRequest, refreshABI, from := g.isSwaggerRequest(req)

	var deployMsg *messages.DeployContract
	var err error
	var isGateway = false
	var prefix, id, addr string
	if strings.HasPrefix(req.URL.Path, "/gateways/") || strings.HasPrefix(req.URL.Path, "/g/") {
		isGateway = true
		prefix = "gateway"
		id = params.ByName("gateway_lookup")
		deployMsg, err = g.rr.loadFactoryForGateway(id, refreshABI)
		if err != nil {
			g.gatewayErrReply(res, req, err, 500)
			return
		} else if deployMsg == nil {
			err = ethconnecterrors.Errorf(ethconnecterrors.RemoteRegistryLookupGatewayNotFound)
			g.gatewayErrReply(res, req, err, 404)
			return
		}
	} else {
		prefix = "instance"
		id = params.ByName("instance_lookup")
		var msg *deployContractWithAddress
		msg, err = g.rr.loadFactoryForInstance(id, refreshABI)
		if err != nil {
			g.gatewayErrReply(res, req, err, 500)
			return
		} else if msg == nil {
			err = ethconnecterrors.Errorf(ethconnecterrors.RemoteRegistryLookupInstanceNotFound)
			g.gatewayErrReply(res, req, err, 404)
			return
		}
		deployMsg = &msg.DeployContract
		addr = msg.Address
	}

	if uiRequest {
		g.writeHTMLForUI(prefix, id, from, isGateway, factoryOnly, res)
	} else if swaggerGen != nil {
		runtimeABI, err := ethbind.API.ABIMarshalingToABIRuntime(deployMsg.ABI)
		if err != nil {
			g.gatewayErrReply(res, req, ethconnecterrors.Errorf(ethconnecterrors.RESTGatewayInvalidABI, err), 400)
			return
		}
		swagger := g.swaggerForRemoteRegistry(swaggerGen, id, addr, factoryOnly, runtimeABI, deployMsg.DevDoc, req.URL.Path)
		g.replyWithSwagger(res, req, swagger, id, from)
	} else if abiRequest {
		log.Infof("<-- %s %s [%d]", req.Method, req.URL, 200)
		res.Header().Set("Content-Type", "application/json")
		res.WriteHeader(200)
		enc := json.NewEncoder(res)
		enc.SetIndent("", "  ")
		enc.Encode(deployMsg.ABI)
	} else {
		ci := &remoteContractInfo{
			ID:      deployMsg.Headers.ID,
			ABI:     deployMsg.ABI,
			Address: addr,
		}
		log.Infof("<-- %s %s [%d]", req.Method, req.URL, 200)
		res.Header().Set("Content-Type", "application/json")
		res.WriteHeader(200)
		enc := json.NewEncoder(res)
		enc.SetIndent("", "  ")
		enc.Encode(ci)
	}
}

func (g *smartContractGW) registerContract(res http.ResponseWriter, req *http.Request, params httprouter.Params) {
	log.Infof("--> %s %s", req.Method, req.URL)

	addrHexNo0x := strings.ToLower(strings.TrimPrefix(params.ByName("address"), "0x"))
	addrCheck, _ := regexp.Compile("^[0-9a-z]{40}$")
	if !addrCheck.MatchString(addrHexNo0x) {
		g.gatewayErrReply(res, req, ethconnecterrors.Errorf(ethconnecterrors.RESTGatewayRegistrationSuppliedInvalidAddress), 404)
		return
	}

	// Note: there is currently no body payload required for the POST

	abiID := params.ByName("abi")
	_, _, err := g.loadDeployMsgByID(abiID)
	if err != nil {
		g.gatewayErrReply(res, req, err, 404)
		return
	}

	registerAs := getFlyParam("register", req, false)
	registeredName := registerAs
	if registeredName == "" {
		registeredName = addrHexNo0x
	}

	contractInfo, err := g.storeNewContractInfo(addrHexNo0x, abiID, registeredName, registerAs)
	if err != nil {
		g.gatewayErrReply(res, req, err, 409)
		return
	}

	status := 201
	log.Infof("<-- %s %s [%d]", req.Method, req.URL, status)
	res.Header().Set("Content-Type", "application/json")
	res.WriteHeader(status)
	json.NewEncoder(res).Encode(&contractInfo)
}

func tempdir() string {
	dir, _ := ioutil.TempDir("", "fly")
	log.Infof("tmpdir/create: %s", dir)
	return dir
}

func cleanup(dir string) {
	log.Infof("tmpdir/cleanup: %s [dir]", dir)
	os.RemoveAll(dir)
}

func (g *smartContractGW) addABI(res http.ResponseWriter, req *http.Request, params httprouter.Params) {
	log.Infof("--> %s %s", req.Method, req.URL)

	if err := req.ParseMultipartForm(maxFormParsingMemory); err != nil {
		g.gatewayErrReply(res, req, ethconnecterrors.Errorf(ethconnecterrors.RESTGatewayCompileContractInvalidFormData, err), 400)
		return
	}

	tempdir := tempdir()
	defer cleanup(tempdir)
	for name, files := range req.MultipartForm.File {
		log.Debugf("multi-part form entry '%s'", name)
		for _, fileHeader := range files {
			if err := g.extractMultiPartFile(tempdir, fileHeader); err != nil {
				g.gatewayErrReply(res, req, err, 400)
				return
			}
		}
	}

	if vs := req.Form["findsolidity"]; len(vs) > 0 {
		var solFiles []string
		filepath.Walk(
			tempdir,
			func(p string, info os.FileInfo, err error) error {
				if strings.HasSuffix(p, ".sol") {
					solFiles = append(solFiles, strings.TrimPrefix(strings.TrimPrefix(p, tempdir), "/"))
				}
				return nil
			})
		log.Infof("<-- %s %s [%d]", req.Method, req.URL, 200)
		res.Header().Set("Content-Type", "application/json")
		res.WriteHeader(200)
		json.NewEncoder(res).Encode(&solFiles)
		return
	}

	abi, err := g.parseABI(req.Form)
	if err != nil {
		g.gatewayErrReply(res, req, ethconnecterrors.Errorf(ethconnecterrors.RESTGatewayCompileContractInvalidFormData, err), 400)
		return
	}

	bytecode, err := g.parseBytecode(req.Form)
	if err != nil {
		g.gatewayErrReply(res, req, ethconnecterrors.Errorf(ethconnecterrors.RESTGatewayCompileContractInvalidFormData, err), 400)
		return
	}

	var preCompiled map[string]*ethbinding.Contract
	if bytecode == nil {
		var err error
		preCompiled, err = g.compileMultipartFormSolidity(tempdir, req)
		if err != nil {
			g.gatewayErrReply(res, req, ethconnecterrors.Errorf(ethconnecterrors.RESTGatewayCompileContractCompileFailed, err), 400)
			return
		}
	}

	if vs := req.Form["findcontracts"]; len(vs) > 0 {
		contractNames := make([]string, 0, len(preCompiled))
		for contractName := range preCompiled {
			contractNames = append(contractNames, contractName)
		}
		log.Infof("<-- %s %s [%d]", req.Method, req.URL, 200)
		res.Header().Set("Content-Type", "application/json")
		res.WriteHeader(200)
		json.NewEncoder(res).Encode(&contractNames)
		return
	}

	msg := &messages.DeployContract{}
	msg.Headers.MsgType = messages.MsgTypeSendTransaction
	msg.Headers.ID = utils.UUIDv4()
	var compiled *eth.CompiledSolidity
	if bytecode == nil && abi == nil {
		var err error
		compiled, err = eth.ProcessCompiled(preCompiled, req.FormValue("contract"), false)
		if err != nil {
			g.gatewayErrReply(res, req, ethconnecterrors.Errorf(ethconnecterrors.RESTGatewayCompileContractPostCompileFailed, err), 400)
			return
		}
	} else {
		msg.ABI = abi
		msg.Compiled = bytecode
	}

	info, err := g.storeDeployableABI(msg, compiled)
	if err != nil {
		g.gatewayErrReply(res, req, err, 500)
		return
	}

	log.Infof("<-- %s %s [%d]", req.Method, req.URL, 200)
	res.Header().Set("Content-Type", "application/json")
	res.WriteHeader(200)
	json.NewEncoder(res).Encode(info)
}

func (g *smartContractGW) parseBytecode(form url.Values) ([]byte, error) {
	v := form["bytecode"]
	if len(v) > 0 {
		b := strings.TrimLeft(v[0], "0x")
		if bytecode, err := hex.DecodeString(b); err != nil {
			log.Errorf("failed to decode hex string: %v", err)
			return nil, err
		} else {
			return bytecode, nil
		}
	}
	return nil, nil
}

func (g *smartContractGW) parseABI(form url.Values) (ethbinding.ABIMarshaling, error) {
	v := form["abi"]
	if len(v) > 0 {
		a := v[0]
		var abi ethbinding.ABIMarshaling
		if err := json.Unmarshal([]byte(a), &abi); err != nil {
			log.Errorf("failed to unmarshal ABI: %v", err.Error())
			return nil, err
		} else {
			return abi, nil
		}
	}
	return nil, nil
}

func (g *smartContractGW) compileMultipartFormSolidity(dir string, req *http.Request) (map[string]*ethbinding.Contract, error) {
	solFiles := []string{}
	rootFiles, err := ioutil.ReadDir(dir)
	if err != nil {
		log.Errorf("Failed to read dir '%s': %s", dir, err)
		return nil, ethconnecterrors.Errorf(ethconnecterrors.RESTGatewayCompileContractExtractedReadFailed)
	}
	for _, file := range rootFiles {
		log.Debugf("multi-part: '%s' [dir=%t]", file.Name(), file.IsDir())
		if strings.HasSuffix(file.Name(), ".sol") {
			solFiles = append(solFiles, file.Name())
		}
	}

	evmVersion := req.FormValue("evm")
	solcArgs := eth.GetSolcArgs(evmVersion)
	if sourceFiles := req.Form["source"]; len(sourceFiles) > 0 {
		solcArgs = append(solcArgs, sourceFiles...)
	} else if len(solFiles) > 0 {
		solcArgs = append(solcArgs, solFiles...)
	} else {
		return nil, ethconnecterrors.Errorf(ethconnecterrors.RESTGatewayCompileContractNoSOL)
	}

	solcVer, err := eth.GetSolc(req.FormValue("compiler"))
	if err != nil {
		return nil, ethconnecterrors.Errorf(ethconnecterrors.RESTGatewayCompileContractSolcVerFail, err)
	}
	solOptionsString := strings.Join(append([]string{solcVer.Path}, solcArgs...), " ")
	log.Infof("Compiling: %s", solOptionsString)
	cmd := exec.Command(solcVer.Path, solcArgs...)

	var stderr, stdout bytes.Buffer
	cmd.Stderr = &stderr
	cmd.Stdout = &stdout
	cmd.Dir = dir
	if err := cmd.Run(); err != nil {
		return nil, ethconnecterrors.Errorf(ethconnecterrors.RESTGatewayCompileContractCompileFailDetails, err, stderr.String())
	}

	compiled, err := ethbind.API.ParseCombinedJSON(stdout.Bytes(), "", solcVer.Version, solcVer.Version, solOptionsString)
	if err != nil {
		return nil, ethconnecterrors.Errorf(ethconnecterrors.RESTGatewayCompileContractSolcOutputProcessFail, err)
	}

	return compiled, nil
}

func (g *smartContractGW) extractMultiPartFile(dir string, file *multipart.FileHeader) error {
	fileName := file.Filename
	if strings.ContainsAny(fileName, "/\\") {
		return ethconnecterrors.Errorf(ethconnecterrors.RESTGatewayCompileContractSlashes)
	}
	in, err := file.Open()
	if err != nil {
		log.Errorf("Failed opening '%s' for reading: %s", fileName, err)
		return ethconnecterrors.Errorf(ethconnecterrors.RESTGatewayCompileContractUnzipRead)
	}
	defer in.Close()
	outFileName := path.Join(dir, fileName)
	out, err := os.OpenFile(outFileName, os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		log.Errorf("Failed opening '%s' for writing: %s", fileName, err)
		return ethconnecterrors.Errorf(ethconnecterrors.RESTGatewayCompileContractUnzipWrite)
	}
	written, err := io.Copy(out, in)
	if err != nil {
		log.Errorf("Failed writing '%s' from multi-part form: %s", fileName, err)
		return ethconnecterrors.Errorf(ethconnecterrors.RESTGatewayCompileContractUnzipCopy)
	}
	log.Debugf("multi-part: '%s' [%dKb]", fileName, written/1024)
	return g.processIfArchive(dir, outFileName)
}

func (g *smartContractGW) processIfArchive(dir, fileName string) error {
	z, err := archiver.ByExtension(fileName)
	if err != nil {
		log.Debugf("multi-part: '%s' not an archive: %s", fileName, err)
		return nil
	}
	err = z.(archiver.Unarchiver).Unarchive(fileName, dir)
	if err != nil {
		return ethconnecterrors.Errorf(ethconnecterrors.RESTGatewayCompileContractUnzip, err)
	}
	return nil
}

// Write out a nice little UI for exercising the Swagger
func (g *smartContractGW) writeHTMLForUI(prefix, id, from string, isGateway, factoryOnly bool, res http.ResponseWriter) {
	fromQuery := ""
	if from != "" {
		fromQuery = "&from=" + url.QueryEscape(from)
	}

	factoryMessage := ""
	if isGateway {
		factoryMessage =
			`       <li><code>POST</code> against <code>/</code> (the constructor) will deploy a new instance of the smart contract
        <ul>
          <li>A dedicated API will be generated for each instance deployed via this API, scoped to that contract Address</li>
        </ul></li>`
	}
	factoryOnlyQuery := ""
	helpHeader := `
  <p>Welcome to the built-in API exerciser of Ethconnect</p>
  `
	hasMethodsMessage := ""
	if factoryOnly {
		factoryOnlyQuery = "&factory"
		helpHeader = `<p>Factory API to deploy contract instances</p>
  <p>Use the <code>[POST]</code> panel below to set the input parameters for your constructor, and tick <code>[TRY]</code> to deploy a contract instance.</p>
  <p>If you want to configure a friendly API path name to invoke your contract, then set the <code>fly-register</code> parameter.</p>`
	} else {
		hasMethodsMessage = `<li><code>GET</code> actions <b>never</b> write to the chain. Even for actions that update state - so you can simulate execution</li>
    <li><code>POST</code> actions against <code>/subscribe</code> paths marked <code>[event]</code> add subscriptions to event streams
    <ul>
      <li>Pre-configure your event streams with actions via the <code>/eventstreams</code> API route on Ethconnect</b></li>
      <li>Once you add a subscription, all matching events will be reliably read, batched and delivered over your event stream</li>
    </ul></li>
    <li>Data type conversion is automatic for all actions an events.
      <ul>
          <li>Numbers are encoded as strings, to avoid loss of precision.</li>
          <li>Byte arrays, including Address fields, are encoded in Hex with an <code>0x</code> prefix</li>
          <li>See the 'Model' of each method and event input/output below for details</li>
      </ul>
    </li>`
	}
	html := `<!DOCTYPE HTML PUBLIC "-//W3C//DTD HTML 4.01//EN" "http://www.w3.org/TR/html4/strict.dtd">
<html>
<head>
  <meta charset="utf-8"> <!-- Important: rapi-doc uses utf8 characters -->
  <script src="https://unpkg.com/rapidoc@7.1.0/dist/rapidoc-min.js"></script>
</head>
<body>
  <rapi-doc 
    spec-url="` + g.conf.BaseURL + "/" + prefix + "s/" + id + "?swagger" + factoryOnlyQuery + fromQuery + `"
    allow-authentication="false"
    allow-spec-url-load="false"
    allow-spec-file-load="false"
    heading-text="Ethconnect REST Gateway"
    header-color="#3842C1"
    theme="light"
		primary-color="#3842C1"
  >
<!-- TODO new image and docs link
    <img 
      slot="logo" 
      src="todo"
      alt="Firefly"
      onclick="window.open('https://todo')"
      style="cursor: pointer; padding-bottom: 2px; margin-left: 25px; margin-right: 10px;"
    />
-->
    <div style="border: #f2f2f2 1px solid; padding: 25px; margin-top: 25px;
      display: flex; flex-direction: row; flex-wrap: wrap;">
      <div style="flex: 1;">
      ` + helpHeader + `
        <p><a href="#quickstart" style="text-decoration: none" onclick="document.getElementById('firefly-quickstart-header').style.display = 'block'; this.style.display = 'none'; return false;">Show additional instructions</a></p>
        <div id="firefly-quickstart-header" style="display: none;">
          <ul>
            <li>Authorization with Firefly Application Credentials has already been performed when loading this page, and is passed to API calls by your browser.</code>
            <li><code>POST</code> actions against Solidity methods will <b>write to the chain</b> unless <code>fly-call</code> is set, or the method is marked <code>[read-only]</code>
            <ul>
              <li>When <code>fly-sync</code> is set, the response will not be returned until the transaction is mined <b>taking a few seconds</b></li>
              <li>When <code>fly-sync</code> is unset, the transaction is reliably streamed to the node over Kafka</li>
              <li>Use the <a href="/replies" target="_blank" style="text-decoration: none">/replies</a> API route on Ethconnect to view receipts for streamed transactions</li>
              <li>Gas limit estimation is performed automatically, unless <code>fly-gas</code> is set.</li>
              <li>During the gas estimation we will return any revert messages if there is a execution failure.</li>
            </ul></li>
            ` + factoryMessage + `
            ` + hasMethodsMessage + `
            <li>Descriptions are taken from the devdoc included in the Solidity code comments</li>
          </ul>        
        </div>
      </div>
      <div style="flex-shrink: 1; margin-left: auto; text-align: center;"">
        <button type="button" style="color: white; background-color: #3942c1;
          font-size: 1rem; border-radius: 4px; cursor: pointer;
          text-transform: uppercase; height: 50px; padding: 0 20px;
          text-align: center; box-sizing: border-box; margin-bottom: 10px;"
          onclick="window.open('` + g.conf.BaseURL + "/" + prefix + "s/" + id + "?swagger&download" + fromQuery + `')">
          Download API
        </button><br/>
<!-- TODO new docs link -->
      </div>
    </div>
  </rapi-doc>
</body> 
</html>
`
	res.Header().Set("Content-Type", "text/html; charset=utf-8")
	res.WriteHeader(200)
	res.Write([]byte(html))
}

// Shutdown performs a clean shutdown
func (g *smartContractGW) Shutdown() {
	if g.sm != nil {
		g.sm.Close()
	}
	if g.rr != nil {
		g.rr.close()
	}
}
