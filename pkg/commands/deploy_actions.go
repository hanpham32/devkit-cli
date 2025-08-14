package commands

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math/big"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Layr-Labs/crypto-libs/pkg/bn254"
	"github.com/Layr-Labs/crypto-libs/pkg/keystore"
	"github.com/Layr-Labs/devkit-cli/config/configs"
	"github.com/Layr-Labs/devkit-cli/config/contexts"
	"github.com/Layr-Labs/devkit-cli/pkg/common"
	"github.com/Layr-Labs/devkit-cli/pkg/common/iface"
	allocationmanager "github.com/Layr-Labs/eigenlayer-contracts/pkg/bindings/AllocationManager"
	ethcommon "github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/urfave/cli/v2"
	"golang.org/x/text/cases"
	"golang.org/x/text/language"
	"gopkg.in/yaml.v3"
)

type DeployContractTransport struct {
	Name    string
	Address string
	ABI     string
}

type DeployContractJson struct {
	Name      string      `json:"name"`
	Address   string      `json:"address"`
	ABI       interface{} `json:"abi"`
	ChainInfo ChainInfo   `json:"chainInfo"`
}

type ChainInfo struct {
	ChainId int64 `json:"chainId"`
}

func StartDeployL1Action(cCtx *cli.Context) error {
	// Get logger
	logger := common.LoggerFromContext(cCtx.Context)
	caser := cases.Title(language.English)

	// Extract vars
	contextName := cCtx.String("context")
	useZeus := cCtx.Bool("use-zeus")

	// Migrate config
	configsMigratedCount, err := configs.MigrateConfig(logger)
	if err != nil {
		logger.Error("config migration failed: %w", err)
	}
	if configsMigratedCount > 0 {
		logger.Info("configs migrated: %d", configsMigratedCount)
	}

	// Migrate contexts
	contextsMigratedCount, err := contexts.MigrateContexts(logger)
	if err != nil {
		logger.Error("context migrations failed: %w", err)
	}
	if contextsMigratedCount > 0 {
		logger.Info("contexts migrated: %d", contextsMigratedCount)
	}

	// Load config for selected context
	var cfg *common.ConfigWithContextConfig
	if contextName == "" {
		cfg, contextName, err = common.LoadDefaultConfigWithContextConfig()
	} else {
		cfg, contextName, err = common.LoadConfigWithContextConfig(contextName)
	}
	if err != nil {
		return fmt.Errorf("failed to load configurations for register key in key registrar: %w", err)
	}

	// Extract context details
	yamlPath, rootNode, contextNode, contextName, err := common.LoadContext(contextName)
	if err != nil {
		return fmt.Errorf("context loading failed: %w", err)
	}

	// Extract context details
	envCtx, ok := cfg.Context[contextName]
	if !ok {
		return fmt.Errorf("context '%s' not found in configuration", contextName)
	}
	l1ChainCfg, ok := envCtx.Chains[common.L1]
	if !ok {
		return fmt.Errorf("L2 chain not found in configuration")
	}

	// Log the action
	logger.Info("Starting L1 (%d) deployment to %s\n", l1ChainCfg.ChainID, contextName)

	// Fetch EigenLayer addresses using Zeus if requested
	if useZeus {
		err = common.UpdateContextWithZeusAddresses(cCtx.Context, logger, contextNode, contextName)
		if err != nil {
			logger.Warn("Failed to fetch addresses from Zeus: %v", err)
			logger.Info("Continuing with addresses from config...")
		} else {
			logger.Info("Successfully updated context with addresses from Zeus")

			// Write yaml back to project directory
			if err := common.WriteYAML(yamlPath, rootNode); err != nil {
				return fmt.Errorf("failed to save updated context: %v", err)
			}
		}
	}

	// Get fork_urls for the provided network (default to sepolia testnet)
	l1ForkUrl, err := common.GetForkUrlDefault(contextName, cfg, common.L1)
	if err != nil {
		return fmt.Errorf("L1 fork URL error %w", err)
	}
	l2ForkUrl, err := common.GetForkUrlDefault(contextName, cfg, common.L2)
	if err != nil {
		return fmt.Errorf("L2 fork URL error: %w", err)
	}

	// Get chains node
	chainsNode := common.GetChildByKey(contextNode, "chains")
	if chainsNode == nil {
		return fmt.Errorf("missing 'chains' key in context")
	}

	// Update RPC URLs for L1 chain
	l1ChainNode := common.GetChildByKey(chainsNode, common.L1)
	if l1ChainNode != nil {
		l1RpcUrlNode := common.GetChildByKey(l1ChainNode, "rpc_url")
		if l1RpcUrlNode != nil {
			l1RpcUrlNode.Value = l1ForkUrl
		}
	}

	// Update RPC URLs for L2 chain
	l2ChainNode := common.GetChildByKey(chainsNode, common.L2)
	if l2ChainNode != nil {
		l2RpcUrlNode := common.GetChildByKey(l2ChainNode, "rpc_url")
		if l2RpcUrlNode != nil {
			l2RpcUrlNode.Value = l2ForkUrl
		}
	}

	// Write yaml back to project directory
	if err := common.WriteYAML(yamlPath, rootNode); err != nil {
		return err
	}

	// Deploy the contracts after starting devnet unless skipped
	if err := DeployL1ContractsAction(cCtx); err != nil {
		return fmt.Errorf("deploy-contracts failed: %w", err)
	}

	// Sleep for 1 second to make sure new context values have been written
	time.Sleep(1 * time.Second)

	// Register AVS with EigenLayer
	logger.Title("Registering AVS with EigenLayer...")
	if !cCtx.Bool("skip-setup") {
		if err := UpdateAVSMetadataAction(cCtx, logger); err != nil {
			return fmt.Errorf("updating AVS metadata failed: %w", err)
		}

		if err := SetAVSRegistrarAction(cCtx, logger); err != nil {
			return fmt.Errorf("setting AVS registrar failed: %w", err)
		}

		if err := CreateAVSOperatorSetsAction(cCtx, logger); err != nil {
			return fmt.Errorf("creating AVS operator sets failed: %w", err)
		}

		if err := ConfigureOpSetCurveTypeAction(cCtx, logger); err != nil {
			return fmt.Errorf("failed to configure OpSet in KeyRegistrar: %w", err)
		}

		if err := CreateGenerationReservationAction(cCtx, logger); err != nil {
			return fmt.Errorf("failed to request op set generation reservation: %w", err)
		}

		if err := RegisterOperatorsToEigenLayerFromConfigAction(cCtx, logger); err != nil {
			return fmt.Errorf("registering operators failed: %w", err)
		}

		if err := RegisterKeyInKeyRegistrarAction(cCtx, logger); err != nil {
			return fmt.Errorf("registering key in key registrar failed: %w", err)
		}

		if err := RegisterOperatorsToAvsFromConfigAction(cCtx, logger); err != nil {
			return fmt.Errorf("registering operators to AVS failed: %w", err)
		}
	} else {
		logger.Info("Skipping AVS setup steps...")
	}

	// L1 Deployment complete
	logger.Info("\n%s L1 Deployment complete\n", caser.String(contextName))

	return nil
}

func StartDeployL2Action(cCtx *cli.Context) error {
	// Get logger
	logger := common.LoggerFromContext(cCtx.Context)
	caser := cases.Title(language.English)

	// Load config for selected context
	contextName := cCtx.String("context")
	var cfg *common.ConfigWithContextConfig
	var err error
	if contextName == "" {
		cfg, contextName, err = common.LoadDefaultConfigWithContextConfig()
	} else {
		cfg, contextName, err = common.LoadConfigWithContextConfig(contextName)
	}
	if err != nil {
		return fmt.Errorf("failed to load configurations: %w", err)
	}

	// Extract context details
	envCtx, ok := cfg.Context[contextName]
	if !ok {
		return fmt.Errorf("context '%s' not found in configuration", contextName)
	}
	l2ChainCfg, ok := envCtx.Chains[common.L2]
	if !ok {
		return fmt.Errorf("L2 chain not found in configuration")
	}
	client, err := ethclient.Dial(l2ChainCfg.RPCURL)
	if err != nil {
		return fmt.Errorf("failed to connect to L2 RPC at %s: %w", l2ChainCfg.RPCURL, err)
	}
	defer client.Close()

	// Log the action
	logger.Info("Starting L2 (%d) deployment to %s\n", l2ChainCfg.ChainID, contextName)

	// Get operatorSets, check curveType, use contractCaller to check getOperatorSetOwner()
	if len(envCtx.OperatorSets) > 0 {
		// Collect AVS address
		avsAddr := envCtx.Avs.Address
		transportedOpSets := 0

		// For each OperatorSets check if the transport has happened yet
		for _, opSet := range envCtx.OperatorSets {
			logger.Debug("Checking owner of AVS: %s and OperatorSet: %d", avsAddr, opSet.OperatorSetID)

			// Collect appropriate CertVerifier based on curveType
			var certVerifierAddr string
			if opSet.CurveType == common.BN254Curve {
				certVerifierAddr = envCtx.EigenLayer.L2.BN254CertificateVerifier
			} else if opSet.CurveType == common.ECDSACurve {
				certVerifierAddr = envCtx.EigenLayer.L2.ECDSACertificateVerifier
			}

			// Pass certVerifierAddr to contractCaller
			if certVerifierAddr != "" {
				contractCaller, err := common.NewContractCaller(
					envCtx.Avs.AVSPrivateKey,
					big.NewInt(int64(l2ChainCfg.ChainID)),
					client,
					ethcommon.HexToAddress(""),
					ethcommon.HexToAddress(""),
					ethcommon.HexToAddress(""),
					ethcommon.HexToAddress(""),
					ethcommon.HexToAddress(""),
					ethcommon.HexToAddress(""),
					ethcommon.HexToAddress(certVerifierAddr),
					logger,
				)

				// Attempt to get owner from appropriate certVerifier
				var owner ethcommon.Address
				if opSet.CurveType == common.BN254Curve {
					owner, err = contractCaller.GetBN254OperatorSetOwner(cCtx.Context, ethcommon.HexToAddress(avsAddr), uint32(opSet.OperatorSetID))
				} else if opSet.CurveType == common.ECDSACurve {
					owner, err = contractCaller.GetECDSAOperatorSetOwner(cCtx.Context, ethcommon.HexToAddress(avsAddr), uint32(opSet.OperatorSetID))
				}

				logger.Debug(" - Owner is set: %s", owner)

				// Test to make sure the transporter has ran before deploying to L2
				if err == nil && owner != ethcommon.HexToAddress("") {
					transportedOpSets++
				}
			}
		}

		// Throw error if any of the operatorSets has not been registered yet
		if transportedOpSets < len(envCtx.OperatorSets) {
			return fmt.Errorf("waiting on transporter, try again soon...")
		}
	}

	// Deploy L2 contracts after transporter has been ran and operatorSetOwner has been set
	if err := DeployL2ContractsAction(cCtx); err != nil && !errors.Is(err, context.Canceled) {
		logger.Error("deploy-l2-contracts failed: %v", err)
		return fmt.Errorf("deploy-l2-contracts failed: %w", err)
	}

	logger.Info("\n%s L2 Deployment complete", caser.String(contextName))

	return nil
}

func DeployL1ContractsAction(cCtx *cli.Context) error {
	// Get logger
	logger := common.LoggerFromContext(cCtx.Context)
	caser := cases.Title(language.English)

	// Check if docker is running, else try to start it
	err := common.EnsureDockerIsRunning(cCtx)
	if err != nil {
		return cli.Exit(err.Error(), 1)
	}

	// Start timing execution runtime
	startTime := time.Now()

	// Run scriptPath from cwd
	const dir = ""

	// Set path for .devkit scripts
	scriptsDir := filepath.Join(".devkit", "scripts")

	// List of scripts we want to call and curry context through
	scriptNames := []string{
		"deployL1Contracts",
		"getOperatorSets",
		"getOperatorRegistrationMetadata",
	}

	// Get contextName from flag (set from config if missing)
	contextName := cCtx.String("context")

	// Check for context
	var yamlPath string
	var rootNode, contextNode *yaml.Node
	if contextName == "" {
		yamlPath, rootNode, contextNode, contextName, err = common.LoadDefaultContext()
	} else {
		yamlPath, rootNode, contextNode, contextName, err = common.LoadContext(contextName)
	}
	if err != nil {
		return fmt.Errorf("context loading failed: %w", err)
	}

	// Loop scripts with cloned context
	for _, name := range scriptNames {
		// if no Operators are available skip registration
		if name == "getOperatorRegistrationMetadata" {
			operators := common.GetChildByKey(contextNode, "operators")
			operatorCount := len(operators.Content)
			if operatorCount == 0 {
				logger.Info("No operators available to register for %s", contextName)
				continue
			}
		}

		// Log the script name that's about to be executed
		logger.Info("Executing script: %s", name)
		// Clone context node and convert to map
		clonedCtxNode := common.CloneNode(contextNode)
		ctxInterface, err := common.NodeToInterface(clonedCtxNode)
		if err != nil {
			return fmt.Errorf("context decode failed: %w", err)
		}

		// Check context is a map
		ctxMap, ok := ctxInterface.(map[string]interface{})
		if !ok {
			return fmt.Errorf("cloned context is not a map")
		}

		// Parse the provided params
		inputJSON, err := json.Marshal(map[string]interface{}{"context": ctxMap})
		if err != nil {
			return fmt.Errorf("marshal context: %w", err)
		}

		// Set path in scriptsDir
		scriptPath := filepath.Join(scriptsDir, name)
		// Expect a JSON response which we will curry to the next call and later save to context
		outMap, err := common.CallTemplateScript(cCtx.Context, logger, dir, scriptPath, common.ExpectJSONResponse, inputJSON)
		if err != nil {
			return fmt.Errorf("%s failed: %w", name, err)
		}

		// Convert to node for merge
		outNode, err := common.InterfaceToNode(outMap)
		if err != nil {
			return fmt.Errorf("%s output invalid: %w", name, err)
		}

		// Merge output into original context node
		common.DeepMerge(contextNode, outNode)
	}

	// Create output .json files for each of the deployed contracts
	contracts := common.GetChildByKey(contextNode, "deployed_l1_contracts")
	if contracts == nil {
		return fmt.Errorf("deployed_l1_contracts node not found")
	}
	var contractsList []DeployContractTransport
	if err := contracts.Decode(&contractsList); err != nil {
		return fmt.Errorf("decode deployed_l1_contracts: %w", err)
	}
	// Get the chainId
	chainId := common.GetChildByKey(contextNode, "chains.l1.chain_id")
	if chainId == nil {
		return fmt.Errorf("chains.l1.chain_id node not found")
	}
	// Title line to split these logs from the main body for easy identification
	logger.Title("Save L1 contract artifacts")
	err = extractContractOutputs(cCtx, contextName, contractsList, chainId.Value)
	if err != nil {
		return fmt.Errorf("failed to write l1 contract artefacts: %w", err)
	}

	// Write yaml back to project directory
	if err := common.WriteYAML(yamlPath, rootNode); err != nil {
		return err
	}

	// Measure how long we ran for
	elapsed := time.Since(startTime).Round(time.Second)
	logger.Info("\n%s L1 contracts deployed successfully in %s", caser.String(contextName), elapsed)
	return nil
}

func DeployL2ContractsAction(cCtx *cli.Context) error {
	// Get logger
	logger := common.LoggerFromContext(cCtx.Context)
	caser := cases.Title(language.English)

	// Check if docker is running, else try to start it
	err := common.EnsureDockerIsRunning(cCtx)
	if err != nil {
		return cli.Exit(err.Error(), 1)
	}

	// Start timing execution runtime
	startTime := time.Now()

	// Run scriptPath from cwd
	const dir = ""

	// Set context/default if missing
	contextName := cCtx.String("context")

	// Set path for .devkit scripts
	scriptsDir := filepath.Join(".devkit", "scripts")

	// List of scripts we want to call and curry context through
	scriptNames := []string{
		"deployL2Contracts",
	}

	// Check for context
	var yamlPath string
	var rootNode, contextNode *yaml.Node
	if contextName == "" {
		yamlPath, rootNode, contextNode, contextName, err = common.LoadDefaultContext()
	} else {
		yamlPath, rootNode, contextNode, contextName, err = common.LoadContext(contextName)
	}
	if err != nil {
		return fmt.Errorf("context loading failed: %w", err)
	}

	// Loop scripts with cloned context
	for _, name := range scriptNames {
		// Log the script name that's about to be executed
		logger.Info("Executing script: %s", name)
		// Clone context node and convert to map
		clonedCtxNode := common.CloneNode(contextNode)
		ctxInterface, err := common.NodeToInterface(clonedCtxNode)
		if err != nil {
			return fmt.Errorf("context decode failed: %w", err)
		}

		// Check context is a map
		ctxMap, ok := ctxInterface.(map[string]interface{})
		if !ok {
			return fmt.Errorf("cloned context is not a map")
		}

		// Parse the provided params
		inputJSON, err := json.Marshal(map[string]interface{}{"context": ctxMap})
		if err != nil {
			return fmt.Errorf("marshal context: %w", err)
		}

		// Set path in scriptsDir
		scriptPath := filepath.Join(scriptsDir, name)
		// Expect a JSON response which we will curry to the next call and later save to context
		outMap, err := common.CallTemplateScript(cCtx.Context, logger, dir, scriptPath, common.ExpectJSONResponse, inputJSON)
		if err != nil {
			return fmt.Errorf("%s failed: %w", name, err)
		}

		// Convert to node for merge
		outNode, err := common.InterfaceToNode(outMap)
		if err != nil {
			return fmt.Errorf("%s output invalid: %w", name, err)
		}

		// Merge output into original context node
		common.DeepMerge(contextNode, outNode)
	}

	// Create output .json files for each of the deployed contracts
	contracts := common.GetChildByKey(contextNode, "deployed_l2_contracts")
	if contracts == nil {
		return fmt.Errorf("deployed_l2_contracts node not found")
	}
	var contractsList []DeployContractTransport
	if err := contracts.Decode(&contractsList); err != nil {
		return fmt.Errorf("decode deployed_l2_contracts: %w", err)
	}
	// Get the chainId
	chainId := common.GetChildByKey(contextNode, "chains.l2.chain_id")
	if chainId == nil {
		return fmt.Errorf("chains.l2.chain_id node not found")
	}
	// Title line to split these logs from the main body for easy identification
	logger.Title("Save L2 contract artifacts")
	err = extractContractOutputs(cCtx, contextName, contractsList, chainId.Value)
	if err != nil {
		return fmt.Errorf("failed to write l2 contract artefacts: %w", err)
	}

	// Write yaml back to project directory
	if err := common.WriteYAML(yamlPath, rootNode); err != nil {
		return err
	}

	// Measure how long we ran for
	elapsed := time.Since(startTime).Round(time.Second)
	logger.Info("\n%s L2 contracts deployed successfully in %s", caser.String(contextName), elapsed)
	return nil

}

func UpdateAVSMetadataAction(cCtx *cli.Context, logger iface.Logger) error {
	// Extract vars
	contextName := cCtx.String("context")
	uri := cCtx.String("uri")

	// Load config for selected context
	var cfg *common.ConfigWithContextConfig
	var err error
	if contextName == "" {
		cfg, contextName, err = common.LoadDefaultConfigWithContextConfig()
	} else {
		cfg, contextName, err = common.LoadConfigWithContextConfig(contextName)
	}
	if err != nil {
		return fmt.Errorf("failed to load configurations: %w", err)
	}

	// Extract context details
	envCtx, ok := cfg.Context[contextName]
	if !ok {
		return fmt.Errorf("context '%s' not found in configuration", contextName)
	}
	l1ChainCfg, ok := envCtx.Chains[common.L1]
	if !ok {
		return fmt.Errorf("L1 chain configuration ('%s') not found in context '%s'", common.L1, contextName)
	}
	client, err := ethclient.Dial(l1ChainCfg.RPCURL)
	if err != nil {
		return fmt.Errorf("failed to connect to L1 RPC at %s: %w", l1ChainCfg.RPCURL, err)
	}
	defer client.Close()
	allocationManagerAddr, delegationManagerAddr, strategyManagerAddr, _, _, _, _, _ := common.GetEigenLayerAddresses(contextName, cfg)

	contractCaller, err := common.NewContractCaller(
		envCtx.Avs.AVSPrivateKey,
		big.NewInt(int64(l1ChainCfg.ChainID)),
		client,
		ethcommon.HexToAddress(allocationManagerAddr),
		ethcommon.HexToAddress(delegationManagerAddr),
		ethcommon.HexToAddress(strategyManagerAddr),
		ethcommon.HexToAddress(""),
		ethcommon.HexToAddress(""),
		ethcommon.HexToAddress(""),
		ethcommon.HexToAddress(""),
		logger,
	)
	if err != nil {
		return fmt.Errorf("failed to create contract caller: %w", err)
	}

	avsAddr := ethcommon.HexToAddress(envCtx.Avs.Address)
	return contractCaller.UpdateAVSMetadata(cCtx.Context, avsAddr, uri)
}

func SetAVSRegistrarAction(cCtx *cli.Context, logger iface.Logger) error {
	// Extract vars
	contextName := cCtx.String("context")

	// Load config for selected context
	var cfg *common.ConfigWithContextConfig
	var err error
	if contextName == "" {
		cfg, contextName, err = common.LoadDefaultConfigWithContextConfig()
	} else {
		cfg, contextName, err = common.LoadConfigWithContextConfig(contextName)
	}
	if err != nil {
		return fmt.Errorf("failed to load configurations: %w", err)
	}

	// Extract context details
	envCtx, ok := cfg.Context[contextName]
	if !ok {
		return fmt.Errorf("context '%s' not found in configuration", contextName)
	}
	l1ChainCfg, ok := envCtx.Chains[common.L1]
	if !ok {
		return fmt.Errorf("L1 chain configuration ('%s') not found in context '%s'", common.L1, contextName)
	}
	client, err := ethclient.Dial(l1ChainCfg.RPCURL)
	if err != nil {
		return fmt.Errorf("failed to connect to L1 RPC at %s: %w", l1ChainCfg.RPCURL, err)
	}
	defer client.Close()
	allocationManagerAddr, delegationManagerAddr, strategyManagerAddr, _, _, _, _, _ := common.GetEigenLayerAddresses(contextName, cfg)

	contractCaller, err := common.NewContractCaller(
		envCtx.Avs.AVSPrivateKey,
		big.NewInt(int64(l1ChainCfg.ChainID)),
		client,
		ethcommon.HexToAddress(allocationManagerAddr),
		ethcommon.HexToAddress(delegationManagerAddr),
		ethcommon.HexToAddress(strategyManagerAddr),
		ethcommon.HexToAddress(""),
		ethcommon.HexToAddress(""),
		ethcommon.HexToAddress(""),
		ethcommon.HexToAddress(""),
		logger,
	)
	if err != nil {
		return fmt.Errorf("failed to create contract caller: %w", err)
	}

	avsAddr := ethcommon.HexToAddress(envCtx.Avs.Address)
	var registrarAddr ethcommon.Address
	logger.Info("Attempting to find AvsRegistrar in deployed contracts...")
	foundInDeployed := false
	for _, contract := range envCtx.DeployedL1Contracts {
		if strings.Contains(strings.ToLower(contract.Name), "avsregistrar") {
			registrarAddr = ethcommon.HexToAddress(contract.Address)
			logger.Info("Found AvsRegistrar: '%s' at address %s", contract.Name, registrarAddr.Hex())
			foundInDeployed = true
			break
		}
	}
	if !foundInDeployed {
		return fmt.Errorf("AvsRegistrar contract not found in deployed l1 contracts for context '%s'", contextName)
	}

	return contractCaller.SetAVSRegistrar(cCtx.Context, avsAddr, registrarAddr)
}

func CreateAVSOperatorSetsAction(cCtx *cli.Context, logger iface.Logger) error {
	// Extract vars
	contextName := cCtx.String("context")

	// Load config for selected context
	var cfg *common.ConfigWithContextConfig
	var err error
	if contextName == "" {
		cfg, contextName, err = common.LoadDefaultConfigWithContextConfig()
	} else {
		cfg, contextName, err = common.LoadConfigWithContextConfig(contextName)
	}
	if err != nil {
		return fmt.Errorf("failed to load configurations: %w", err)
	}

	// Extract context details
	envCtx, ok := cfg.Context[contextName]
	if !ok {
		return fmt.Errorf("context '%s' not found in configuration", contextName)
	}
	l1ChainCfg, ok := envCtx.Chains[common.L1]
	if !ok {
		return fmt.Errorf("L1 chain configuration ('%s') not found in context '%s'", common.L1, contextName)
	}
	client, err := ethclient.Dial(l1ChainCfg.RPCURL)
	if err != nil {
		return fmt.Errorf("failed to connect to L1 RPC at %s: %w", l1ChainCfg.RPCURL, err)
	}
	defer client.Close()
	allocationManagerAddr, delegationManagerAddr, strategyManagerAddr, _, _, _, _, _ := common.GetEigenLayerAddresses(contextName, cfg)

	contractCaller, err := common.NewContractCaller(
		envCtx.Avs.AVSPrivateKey,
		big.NewInt(int64(l1ChainCfg.ChainID)),
		client,
		ethcommon.HexToAddress(allocationManagerAddr),
		ethcommon.HexToAddress(delegationManagerAddr),
		ethcommon.HexToAddress(strategyManagerAddr),
		ethcommon.HexToAddress(""),
		ethcommon.HexToAddress(""),
		ethcommon.HexToAddress(""),
		ethcommon.HexToAddress(""),
		logger,
	)
	if err != nil {
		return fmt.Errorf("failed to create contract caller: %w", err)
	}

	avsAddr := ethcommon.HexToAddress(envCtx.Avs.Address)
	if len(envCtx.OperatorSets) == 0 {
		logger.Info("No operator sets to create.")
		return nil
	}
	createSetParams := make([]allocationmanager.IAllocationManagerTypesCreateSetParams, len(envCtx.OperatorSets))
	for i, opSet := range envCtx.OperatorSets {
		strategies := make([]ethcommon.Address, len(opSet.Strategies))
		for j, strategy := range opSet.Strategies {
			strategies[j] = ethcommon.HexToAddress(strategy.StrategyAddress)
		}
		createSetParams[i] = allocationmanager.IAllocationManagerTypesCreateSetParams{
			OperatorSetId: uint32(opSet.OperatorSetID),
			Strategies:    strategies,
		}
	}

	logger.Info("creating operatorSets")

	return contractCaller.CreateOperatorSets(cCtx.Context, avsAddr, createSetParams)
}

func RegisterOperatorsToEigenLayerFromConfigAction(cCtx *cli.Context, logger iface.Logger) error {
	// Extract vars
	contextName := cCtx.String("context")

	// Load config for selected context
	var cfg *common.ConfigWithContextConfig
	var err error
	if contextName == "" {
		cfg, contextName, err = common.LoadDefaultConfigWithContextConfig()
	} else {
		cfg, contextName, err = common.LoadConfigWithContextConfig(contextName)
	}
	if err != nil {
		return fmt.Errorf("failed to load configurations for operator registration: %w", err)
	}

	// Extract context details
	envCtx, ok := cfg.Context[contextName]
	if !ok {
		return fmt.Errorf("context '%s' not found in configuration", contextName)
	}

	logger.Info("Registering operators with EigenLayer...")
	if len(envCtx.OperatorRegistrations) == 0 || len(envCtx.Operators) == 0 {
		logger.Info("No operator registrations found in context, skipping operator registration.")
		return nil
	}

	for _, opReg := range envCtx.OperatorRegistrations {
		logger.Info("Processing registration for operator at address %s", opReg.Address)
		if err := registerOperatorEL(cCtx, opReg.Address, logger); err != nil {
			logger.Error("Failed to register operator %s with EigenLayer: %v. Continuing...", opReg.Address, err)
			continue
		}
	}
	logger.Info("Operators registration with EigenLayer completed.")
	return nil
}

func RegisterOperatorsToAvsFromConfigAction(cCtx *cli.Context, logger iface.Logger) error {
	// Extract vars
	contextName := cCtx.String("context")

	// Load config for selected context
	var cfg *common.ConfigWithContextConfig
	var err error
	if contextName == "" {
		cfg, contextName, err = common.LoadDefaultConfigWithContextConfig()
	} else {
		cfg, contextName, err = common.LoadConfigWithContextConfig(contextName)
	}
	if err != nil {
		return fmt.Errorf("failed to load configurations for operator registration: %w", err)
	}

	// Extract context details
	envCtx, ok := cfg.Context[contextName]
	if !ok {
		return fmt.Errorf("context '%s' not found in configuration", contextName)
	}

	logger.Info("Registering operators to AVS from config...")
	if len(envCtx.OperatorRegistrations) == 0 || len(envCtx.Operators) == 0 {
		logger.Info("No operator registrations found in context, skipping operator registration.")
		return nil
	}

	for _, opReg := range envCtx.OperatorRegistrations {
		logger.Info("Processing avs registration for operator at address %s", opReg.Address)
		if err := registerOperatorAVS(cCtx, logger, opReg.Address, uint32(opReg.OperatorSetID), opReg.Payload); err != nil {
			logger.Error("Failed to register operator %s for AVS: %v. Continuing...", opReg.Address, err)
			continue
		}
		logger.Info("Successfully registered operator %s for OperatorSetID %d", opReg.Address, opReg.OperatorSetID)
	}
	return nil
}

func FetchZeusAddressesAction(cCtx *cli.Context) error {
	logger := common.LoggerFromContext(cCtx.Context)

	// Extract vars
	contextName := cCtx.String("context")

	// Check for context
	var yamlPath string
	var rootNode, contextNode *yaml.Node
	var err error
	if contextName == "" {
		yamlPath, rootNode, contextNode, contextName, err = common.LoadDefaultContext()
	} else {
		yamlPath, rootNode, contextNode, contextName, err = common.LoadContext(contextName)
	}
	if err != nil {
		return fmt.Errorf("context loading failed: %w", err)
	}

	// Update the context with the fetched addresses
	err = common.UpdateContextWithZeusAddresses(cCtx.Context, logger, contextNode, contextName)
	if err != nil {
		return fmt.Errorf("failed to update context (%s) with Zeus addresses: %w", contextName, err)
	}

	// Write yaml back to project directory
	if err := common.WriteYAML(yamlPath, rootNode); err != nil {
		return fmt.Errorf("failed to save updated context: %v", err)
	}

	logger.Info("Successfully updated %s context with EigenLayer core addresses", contextName)
	return nil
}

func extractContractOutputs(cCtx *cli.Context, context string, contractsList []DeployContractTransport, chainId string) error {
	logger := common.LoggerFromContext(cCtx.Context)

	// Push contract artefacts to ./contracts/outputs
	outDir := filepath.Join("contracts", "outputs", context)
	if err := os.MkdirAll(outDir, fs.ModePerm); err != nil {
		return fmt.Errorf("create output dir: %w", err)
	}

	// Convert chainId to int
	chainIdInt, err := strconv.ParseInt(chainId, 10, 64)
	if err != nil {
		return fmt.Errorf("failed to convert chainId: %w", err)
	}

	// For each contract extract details and produce json file in outputs/<context>/<contract.name>.json
	for _, contract := range contractsList {
		nameVal := contract.Name
		addressVal := contract.Address
		abiVal := contract.ABI

		// Skip storing artefacts if values are missing
		if nameVal == "" || addressVal == "" || abiVal == "" {
			continue
		}

		// Read the ABI file
		raw, err := os.ReadFile(abiVal)
		// if abi is missing then we cannot write outputs, skip to next entry
		if err != nil {
			logger.Error("read ABI for %s (%s) from %q: %w", nameVal, addressVal, abiVal, err)
			continue
		}

		// Temporary struct to pick only the "abi" field from the artifact
		var abi struct {
			ABI interface{} `json:"abi"`
		}
		if err := json.Unmarshal(raw, &abi); err != nil {
			return fmt.Errorf("unmarshal artifact JSON for %s (%s) failed: %w", nameVal, addressVal, err)
		}

		// Check if provided abi is valid
		if err := common.IsValidABI(abi.ABI); err != nil {
			return fmt.Errorf("ABI for %s (%s) is invalid: %v", nameVal, addressVal, err)
		}

		// Build the output struct
		out := DeployContractJson{
			Name:    nameVal,
			Address: addressVal,
			ABI:     abi.ABI,
			ChainInfo: ChainInfo{
				ChainId: chainIdInt,
			},
		}

		// Marshal with indentation
		data, err := json.MarshalIndent(out, "", "  ")
		if err != nil {
			return fmt.Errorf("marshal output for %s (%s): %w", nameVal, addressVal, err)
		}

		// Write to ./contracts/outputs/<context>/<name>.json
		outPath := filepath.Join(outDir, nameVal+".json")
		if err := os.WriteFile(outPath, data, 0o644); err != nil {
			return fmt.Errorf("write output to %s (%s): %w", outPath, addressVal, err)
		}

		logger.Info("Written contract output: %s\n", outPath)
	}
	return nil
}

// ConfigureOpSetCurveType
func ConfigureOpSetCurveTypeAction(cCtx *cli.Context, logger iface.Logger) error {
	// Extract vars
	contextName := cCtx.String("context")

	// Load config for selected context
	var cfg *common.ConfigWithContextConfig
	var err error
	if contextName == "" {
		cfg, contextName, err = common.LoadDefaultConfigWithContextConfig()
	} else {
		cfg, contextName, err = common.LoadConfigWithContextConfig(contextName)
	}
	if err != nil {
		return fmt.Errorf("failed to load configurations for configure op set curve type: %w", err)
	}

	// Extract context details
	envCtx, ok := cfg.Context[contextName]
	if !ok {
		return fmt.Errorf("context '%s' not found in configuration", contextName)
	}

	l1Cfg, ok := envCtx.Chains[common.L1]
	if !ok {
		return fmt.Errorf("failed to get l1 chain config for context '%s'", contextName)
	}

	client, err := ethclient.Dial(l1Cfg.RPCURL)
	if err != nil {
		return fmt.Errorf("failed to connect to L1 RPC: %w", err)
	}
	defer client.Close()

	avsAddress := ethcommon.HexToAddress(envCtx.Avs.Address)
	avsPrivateKeyOrGivenPermissionByAvs := envCtx.Avs.AVSPrivateKey
	_, _, _, keyRegistrarAddr, _, _, _, _ := common.GetEigenLayerAddresses(contextName, cfg)

	contractCaller, err := common.NewContractCaller(
		avsPrivateKeyOrGivenPermissionByAvs,
		big.NewInt(int64(l1Cfg.ChainID)),
		client,
		ethcommon.HexToAddress(""),
		ethcommon.HexToAddress(""),
		ethcommon.HexToAddress(""),
		ethcommon.HexToAddress(keyRegistrarAddr),
		ethcommon.HexToAddress(""),
		ethcommon.HexToAddress(""),
		ethcommon.HexToAddress(""),
		logger,
	)
	if err != nil {
		return fmt.Errorf("failed to create contract caller: %w", err)
	}
	// For each created operator set, configure the curve type
	for _, opSet := range envCtx.OperatorSets {
		// Determine the curve type constant
		var curveTypeValue uint8
		switch opSet.CurveType {
		case common.ECDSACurve:
			curveTypeValue = common.CURVE_TYPE_KEY_REGISTRAR_ECDSA
		case common.BN254Curve:
			curveTypeValue = common.CURVE_TYPE_KEY_REGISTRAR_BN254
		case common.UnknownCurve:
			return fmt.Errorf("unknown curve type for operator set %d - please specify either 'ECDSA' or 'BN254'", opSet.OperatorSetID)
		default:
			// Default to BN254 if not specified
			curveTypeValue = common.CURVE_TYPE_KEY_REGISTRAR_BN254
		}

		logger.Info("Configuring curve type %s for operator set %d", opSet.CurveType, opSet.OperatorSetID)

		// Check current curveType - throw if we are attempting to change it

		// Configure the curve type
		err = contractCaller.ConfigureOpSetCurveType(
			cCtx.Context, avsAddress,
			uint32(opSet.OperatorSetID),
			curveTypeValue,
		)
		if err != nil {
			return fmt.Errorf("failed to configure curve type for operator set %v: %w", opSet.OperatorSetID, err)
		}
		logger.Info("Successfully configured curve type %s for operator set %d", string(opSet.CurveType), opSet.OperatorSetID)
	}

	return nil
}

func CreateGenerationReservationAction(cCtx *cli.Context, logger iface.Logger) error {
	// Extract vars
	contextName := cCtx.String("context")

	// Load config for selected context
	var cfg *common.ConfigWithContextConfig
	var err error
	if contextName == "" {
		cfg, contextName, err = common.LoadDefaultConfigWithContextConfig()
	} else {
		cfg, contextName, err = common.LoadConfigWithContextConfig(contextName)
	}
	if err != nil {
		return fmt.Errorf("failed to load configurations for request op set generation reservation: %w", err)
	}

	// Extract context details
	envCtx, ok := cfg.Context[contextName]
	if !ok {
		return fmt.Errorf("context '%s' not found in configuration", contextName)
	}

	l1Cfg, ok := envCtx.Chains[common.L1]
	if !ok {
		return fmt.Errorf("failed to get l1 chain config for context '%s'", contextName)
	}

	client, err := ethclient.Dial(l1Cfg.RPCURL)
	if err != nil {
		return fmt.Errorf("failed to connect to L1 RPC: %w", err)
	}
	defer client.Close()

	avsAddress := ethcommon.HexToAddress(envCtx.Avs.Address)
	avsPrivateKeyOrGivenPermissionByAvs := envCtx.Avs.AVSPrivateKey
	_, _, _, keyRegistrarAddr, crossChainRegistryAddr, bn254TableCalculatorAddr, ecdsaTableCalculatorAddr, _ := common.GetEigenLayerAddresses(contextName, cfg)

	contractCaller, err := common.NewContractCaller(
		avsPrivateKeyOrGivenPermissionByAvs,
		big.NewInt(int64(l1Cfg.ChainID)),
		client,
		ethcommon.HexToAddress(""),
		ethcommon.HexToAddress(""),
		ethcommon.HexToAddress(""),
		ethcommon.HexToAddress(keyRegistrarAddr),
		ethcommon.HexToAddress(crossChainRegistryAddr),
		ethcommon.HexToAddress(""),
		ethcommon.HexToAddress(""),
		logger,
	)
	if err != nil {
		return fmt.Errorf("failed to create contract caller: %w", err)
	}

	// Wait 1 block
	time.Sleep(12 * time.Second)

	// Create reservations for each opset
	for _, opSet := range envCtx.OperatorSets {
		// Select appropriate table calculator address
		var tableCalculatorAddr string
		if opSet.CurveType == common.BN254Curve {
			tableCalculatorAddr = bn254TableCalculatorAddr
		} else if opSet.CurveType == common.ECDSACurve {
			tableCalculatorAddr = ecdsaTableCalculatorAddr
		}
		// Create reservation against appropriate TableCalculator
		err = contractCaller.CreateGenerationReservation(cCtx.Context, uint32(opSet.OperatorSetID), ethcommon.HexToAddress(tableCalculatorAddr), avsAddress)
		if err != nil {
			return fmt.Errorf("failed to request op set generation reservation: %w", err)
		}
	}

	logger.Info("Successfully requested op set generation reservation")

	return nil
}

func RegisterKeyInKeyRegistrarAction(cCtx *cli.Context, logger iface.Logger) error {
	// Extract vars
	contextName := cCtx.String("context")

	// Load config for selected context
	var cfg *common.ConfigWithContextConfig
	var err error
	if contextName == "" {
		cfg, contextName, err = common.LoadDefaultConfigWithContextConfig()
	} else {
		cfg, contextName, err = common.LoadConfigWithContextConfig(contextName)
	}
	if err != nil {
		return fmt.Errorf("failed to load configurations for register key in key registrar: %w", err)
	}

	// Extract context details
	envCtx, ok := cfg.Context[contextName]
	if !ok {
		return fmt.Errorf("context '%s' not found in configuration", contextName)
	}

	l1Cfg, ok := envCtx.Chains[common.L1]
	if !ok {
		return fmt.Errorf("failed to get l1 chain config for context '%s'", contextName)
	}

	client, err := ethclient.Dial(l1Cfg.RPCURL)
	if err != nil {
		return fmt.Errorf("failed to connect to L1 RPC: %w", err)
	}

	avsAddress := ethcommon.HexToAddress(envCtx.Avs.Address)
	_, _, _, keyRegistrarAddr, _, _, _, _ := common.GetEigenLayerAddresses(contextName, cfg)

	for _, op := range envCtx.OperatorRegistrations {

		for _, operator := range envCtx.Operators {

			if op.Address == operator.Address {
				operatorPrivateKey, err := loadOperatorECDSAKey(operator)
				if err != nil {
					return fmt.Errorf("failed to load ECDSA key for operator %s: %w", operator.Address, err)
				}
				operatorAddress := ethcommon.HexToAddress(op.Address)
				contractCaller, err := common.NewContractCaller(
					operatorPrivateKey,
					big.NewInt(int64(l1Cfg.ChainID)),
					client,
					ethcommon.HexToAddress(""),
					ethcommon.HexToAddress(""),
					ethcommon.HexToAddress(""),
					ethcommon.HexToAddress(keyRegistrarAddr),
					ethcommon.HexToAddress(""),
					ethcommon.HexToAddress(""),
					ethcommon.HexToAddress(""),
					logger,
				)
				if err != nil {
					return fmt.Errorf("failed to create contract caller: %w", err)
				}

				var blskeystorePath, blskeystorePassword string
				for _, ks := range operator.Keystores {
					if ks.OperatorSet == op.OperatorSetID {
						blskeystorePath = ks.BlsKeystorePath
						blskeystorePassword = ks.BlsKeystorePassword
						break
					}
				}

				if blskeystorePath == "" {
					return fmt.Errorf("no bls keystore found for OperatorSet %d", op.OperatorSetID)
				}

				keystoreData, err := keystore.LoadKeystoreFile(blskeystorePath)
				if err != nil {
					return fmt.Errorf("failed to load keystore %q: %w", blskeystorePath, err)
				}
				if err != nil {
					return fmt.Errorf("failed to load the keystore file from given path %s error %w", blskeystorePath, err)
				}

				privateKey, err := keystoreData.GetBN254PrivateKey(blskeystorePassword)
				if err != nil {
					return fmt.Errorf("failed to extract the private key from the keystore file")

				}

				keyData, err := contractCaller.EncodeBN254KeyData(privateKey.Public())
				if err != nil {
					return fmt.Errorf("failed to encode key data: %w", err)
				}

				messageHash, err := contractCaller.GetOperatorRegistrationMessageHash(cCtx.Context, operatorAddress, avsAddress, uint32(op.OperatorSetID), keyData)
				if err != nil {
					return fmt.Errorf("failed to get operator registration message hash: %w", err)
				}

				signature, err := privateKey.SignSolidityCompatible(messageHash)
				if err != nil {
					return fmt.Errorf("failed to sign message hash: %w", err)
				}

				bn254Signature := bn254.Signature(*signature)

				err = contractCaller.RegisterKeyInKeyRegistrar(cCtx.Context, operatorAddress, avsAddress, uint32(op.OperatorSetID), keyData, bn254Signature)
				if err != nil {
					return fmt.Errorf("failed to register key in key registrar: %w", err)
				}
				logger.Info("Successfully registered key in key registrar for operator %s", operator.Address)
			}
		}

	}
	logger.Info("Successfully registered keys in key registrar")
	return nil
}
