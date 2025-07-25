package commands

import (
	"fmt"
	"path/filepath"

	"github.com/Layr-Labs/devkit-cli/config/configs"
	"github.com/Layr-Labs/devkit-cli/config/contexts"
	"github.com/Layr-Labs/devkit-cli/pkg/common"
	"gopkg.in/yaml.v3"

	"github.com/urfave/cli/v2"
)

// BuildCommand defines the "build" command
var BuildCommand = &cli.Command{
	Name:  "build",
	Usage: "Compiles AVS components (smart contracts via Foundry, Go binaries for operators/aggregators)",
	Flags: append([]cli.Flag{
		&cli.StringFlag{
			Name:  "context",
			Usage: "Select the context to use in this command (devnet, testnet or mainnet)",
		},
	}, common.GlobalFlags...),
	Action: func(cCtx *cli.Context) error {
		logger := common.LoggerFromContext(cCtx.Context)

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

		// Run scriptPath from cwd
		const dir = ""

		// Get the config (based on if we're in a test or not)
		var cfg *common.ConfigWithContextConfig

		// Load selected context
		contextName := cCtx.String("context")

		// Load from file if not in context
		cfg, err = common.LoadConfigWithContextConfig(contextName)
		if err != nil {
			return err
		}
		if contextName == "" {
			contextName = cfg.Config.Project.Context
		}

		// Handle version increment
		version := cfg.Context[contextName].Artifact.Version
		if version == "" {
			version = "0"
		}

		logger.Debug("Project Name: %s", cfg.Config.Project.Name)
		logger.Debug("Building AVS components...")

		// All scripts contained here
		scriptsDir := filepath.Join(".devkit", "scripts")

		// Load context JSON to pass to script
		contextJSON, err := common.LoadRawContext(contextName)
		if err != nil {
			return fmt.Errorf("failed to load context: %w", err)
		}

		// Execute build via .devkit scripts with project name
		output, err := common.CallTemplateScript(cCtx.Context, logger, dir, filepath.Join(scriptsDir, "build"), common.ExpectJSONResponse,
			[]byte("--image"),
			[]byte(cfg.Config.Project.Name),
			[]byte("--tag"),
			[]byte(version),
			contextJSON,
		)
		if err != nil {
			logger.Error("Build script failed with error: %v", err)
			return fmt.Errorf("build failed: %w", err)
		}

		// Load the context yaml file
		contextPath := filepath.Join("config", "contexts", fmt.Sprintf("%s.yaml", contextName))
		contextNode, err := common.LoadYAML(contextPath)
		if err != nil {
			return fmt.Errorf("failed to load context yaml: %w", err)
		}

		// Get the root node (first content node)
		rootNode := contextNode.Content[0]

		// Get or create the context section
		contextSection := common.GetChildByKey(rootNode, "context")
		if contextSection == nil {
			contextSection = &yaml.Node{Kind: yaml.MappingNode}
			rootNode.Content = append(rootNode.Content,
				&yaml.Node{Kind: yaml.ScalarNode, Value: "context"},
				contextSection,
			)
		}

		// Update artifact in context
		if err := updateArtifactFromBuild(contextSection, output); err != nil {
			return fmt.Errorf("failed to update artifact: %w", err)
		}

		// Write the merged yaml back to file
		if err := common.WriteYAML(contextPath, contextNode); err != nil {
			return fmt.Errorf("failed to write merged yaml: %w", err)
		}

		logger.Info("Build completed successfully")
		return nil
	},
}

// updateArtifactFromBuild updates the artifactId and component fields in the context yaml file
func updateArtifactFromBuild(contextSection *yaml.Node, buildOutput interface{}) error {
	// Convert build output to map for easier access
	outputMap, ok := buildOutput.(map[string]interface{})
	if !ok {
		return fmt.Errorf("build output is not a map")
	}

	// Get or create artifact section
	artifactSection := common.GetChildByKey(contextSection, "artifact")
	if artifactSection == nil {
		artifactSection = &yaml.Node{Kind: yaml.MappingNode}
		common.SetMappingValue(contextSection,
			&yaml.Node{Kind: yaml.ScalarNode, Value: "artifact"},
			artifactSection)
	}

	// Update artifact fields from build output
	if artifact, ok := outputMap["artifact"].(map[string]interface{}); ok {
		// Update artifactId if present
		if artifactId, exists := artifact["artifactId"]; exists {
			common.SetMappingValue(artifactSection,
				&yaml.Node{Kind: yaml.ScalarNode, Value: "artifactId"},
				&yaml.Node{Kind: yaml.ScalarNode, Value: artifactId.(string), Tag: "!!str"})
		}

		// Update component if present
		if component, exists := artifact["component"]; exists {
			common.SetMappingValue(artifactSection,
				&yaml.Node{Kind: yaml.ScalarNode, Value: "component"},
				&yaml.Node{Kind: yaml.ScalarNode, Value: component.(string), Tag: "!!str"})
		}
	}

	return nil
}
