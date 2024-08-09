package combustion

import (
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/suse-edge/edge-image-builder/pkg/fileio"
	"github.com/suse-edge/edge-image-builder/pkg/image"
	"github.com/suse-edge/edge-image-builder/pkg/kubernetes"
	"github.com/suse-edge/edge-image-builder/pkg/log"
	"github.com/suse-edge/edge-image-builder/pkg/template"
	"go.uber.org/zap"
	"gopkg.in/yaml.v3"
)

const (
	k8sComponentName = "kubernetes"

	k8sDir          = "kubernetes"
	k8sConfigDir    = "config"
	k8sInstallDir   = "install"
	k8sImagesDir    = "images"
	k8sManifestsDir = "manifests"

	helmDir       = "helm"
	helmValuesDir = "values"
	helmCertsDir  = "certs"

	k8sInitServerConfigFile = "init_server.yaml"
	k8sServerConfigFile     = "server.yaml"
	k8sAgentConfigFile      = "agent.yaml"

	k8sInstallScript = "20-k8s-install.sh"
)

var (
	//go:embed templates/rke2-single-node-installer.sh.tpl
	rke2SingleNodeInstaller string

	//go:embed templates/rke2-multi-node-installer.sh.tpl
	rke2MultiNodeInstaller string

	//go:embed templates/k3s-single-node-installer.sh.tpl
	k3sSingleNodeInstaller string

	//go:embed templates/k3s-multi-node-installer.sh.tpl
	k3sMultiNodeInstaller string

	//go:embed templates/k8s-vip.yaml.tpl
	k8sVIPManifest string
)

func (c *Combustion) configureKubernetes(ctx *image.Context) ([]string, error) {
	version := ctx.ImageDefinition.Kubernetes.Version

	if version == "" {
		log.AuditComponentSkipped(k8sComponentName)
		return nil, nil
	}

	configureFunc := c.kubernetesConfigurator(version)
	if configureFunc == nil {
		log.AuditComponentFailed(k8sComponentName)
		return nil, fmt.Errorf("cannot configure kubernetes version: %s", version)
	}

	// Show a message to the user to indicate that the Kubernetes component
	// is usually taking longer to complete due to downloading files
	log.Audit("Configuring Kubernetes component...")

	if kubernetes.ServersCount(ctx.ImageDefinition.Kubernetes.Nodes) == 2 {
		log.Audit("WARNING: Kubernetes clusters consisting of two server nodes cannot form a highly available architecture")
		zap.S().Warn("Kubernetes cluster of two server nodes has been requested")
	}

	configDir := generateComponentPath(ctx, k8sDir)
	configPath := filepath.Join(configDir, k8sConfigDir)

	cluster, err := kubernetes.NewCluster(&ctx.ImageDefinition.Kubernetes, configPath)
	if err != nil {
		log.AuditComponentFailed(k8sComponentName)
		return nil, fmt.Errorf("initialising cluster config: %w", err)
	}

	artefactsPath := kubernetesArtefactsPath(ctx)
	if err = os.MkdirAll(artefactsPath, os.ModePerm); err != nil {
		return nil, fmt.Errorf("creating kubernetes artefacts path: %w", err)
	}

	if err = storeKubernetesClusterConfig(cluster, artefactsPath); err != nil {
		log.AuditComponentFailed(k8sComponentName)
		return nil, fmt.Errorf("storing cluster config: %w", err)
	}

	script, err := configureFunc(ctx, cluster)
	if err != nil {
		log.AuditComponentFailed(k8sComponentName)
		return nil, fmt.Errorf("configuring kubernetes components: %w", err)
	}

	log.AuditComponentSuccessful(k8sComponentName)
	return []string{script}, nil
}

func (c *Combustion) kubernetesConfigurator(version string) func(*image.Context, *kubernetes.Cluster) (string, error) {
	switch {
	case strings.Contains(version, image.KubernetesDistroRKE2):
		return c.configureRKE2
	case strings.Contains(version, image.KubernetesDistroK3S):
		return c.configureK3S
	default:
		return nil
	}
}

func (c *Combustion) downloadKubernetesInstallScript(ctx *image.Context, distribution string) (string, error) {
	path := kubernetesArtefactsPath(ctx)

	installScript, err := c.KubernetesScriptDownloader.DownloadInstallScript(distribution, path)
	if err != nil {
		return "", fmt.Errorf("downloading install script: %w", err)
	}

	return prependArtefactPath(filepath.Join(k8sDir, installScript)), nil
}

func (c *Combustion) configureK3S(ctx *image.Context, cluster *kubernetes.Cluster) (string, error) {
	zap.S().Info("Configuring K3s cluster")

	installScript, err := c.downloadKubernetesInstallScript(ctx, image.KubernetesDistroK3S)
	if err != nil {
		return "", fmt.Errorf("downloading k3s install script: %w", err)
	}

	binaryPath, imagesPath, err := c.downloadK3sArtefacts(ctx)
	if err != nil {
		return "", fmt.Errorf("downloading k3s artefacts: %w", err)
	}

	manifestsPath, err := c.configureManifests(ctx)
	if err != nil {
		return "", fmt.Errorf("configuring kubernetes manifests: %w", err)
	}

	templateValues := map[string]any{
		"installScript":   installScript,
		"apiVIP":          ctx.ImageDefinition.Kubernetes.Network.APIVIP,
		"apiHost":         ctx.ImageDefinition.Kubernetes.Network.APIHost,
		"binaryPath":      binaryPath,
		"imagesPath":      imagesPath,
		"manifestsPath":   manifestsPath,
		"configFilePath":  prependArtefactPath(k8sDir),
		"registryMirrors": prependArtefactPath(filepath.Join(k8sDir, registryMirrorsFileName)),
	}

	singleNode := len(ctx.ImageDefinition.Kubernetes.Nodes) < 2
	if singleNode {
		if ctx.ImageDefinition.Kubernetes.Network.APIVIP == "" {
			zap.S().Info("Virtual IP address for k3s cluster is not provided and will not be configured")
		} else {
			log.Audit("WARNING: A Virtual IP address for the k3s cluster has been provided. " +
				"An external IP address for the Ingress Controller (Traefik) must be manually configured.")
			zap.S().Warn("Virtual IP address for k3s cluster is requested and will invalidate Traefik configuration")
		}

		templateValues["configFile"] = k8sServerConfigFile

		return storeKubernetesInstaller(ctx, "single-node-k3s", k3sSingleNodeInstaller, templateValues)
	}

	log.Audit("WARNING: An external IP address for the Ingress Controller (Traefik) must be manually configured in multi-node clusters.")
	zap.S().Warn("Virtual IP address for k3s cluster is necessary for multi node clusters and will invalidate Traefik configuration")

	templateValues["nodes"] = ctx.ImageDefinition.Kubernetes.Nodes
	templateValues["initialiser"] = cluster.InitialiserName
	templateValues["initialiserConfigFile"] = k8sInitServerConfigFile

	return storeKubernetesInstaller(ctx, "multi-node-k3s", k3sMultiNodeInstaller, templateValues)
}

func (c *Combustion) downloadK3sArtefacts(ctx *image.Context) (binaryPath, imagesPath string, err error) {
	imagesPath = filepath.Join(k8sDir, k8sImagesDir)
	imagesDestination := filepath.Join(ctx.ArtefactsDir, imagesPath)
	if err = os.MkdirAll(imagesDestination, os.ModePerm); err != nil {
		return "", "", fmt.Errorf("creating kubernetes images dir: %w", err)
	}

	installPath := filepath.Join(k8sDir, k8sInstallDir)
	installDestination := filepath.Join(ctx.ArtefactsDir, installPath)
	if err = os.MkdirAll(installDestination, os.ModePerm); err != nil {
		return "", "", fmt.Errorf("creating kubernetes install dir: %w", err)
	}

	if err = c.KubernetesArtefactDownloader.DownloadK3sArtefacts(
		ctx.ImageDefinition.Image.Arch,
		ctx.ImageDefinition.Kubernetes.Version,
		installDestination,
		imagesDestination,
	); err != nil {
		return "", "", fmt.Errorf("downloading artefacts: %w", err)
	}

	// As of Jan 2024 / k3s 1.29, the only install artefact is the k3s binary itself.
	// However, the release page has different names for it depending on the architecture:
	// "k3s" for x86_64 and "k3s-arm64" for aarch64.
	// It is too inconvenient to rename it in the artefact downloader and since technically
	// aarch64 is not supported yet, building abstractions around this only scenario is not worth it.
	// Can (and probably should) be revisited later.
	entries, err := os.ReadDir(installDestination)
	if err != nil {
		return "", "", fmt.Errorf("reading k3s install path: %w", err)
	}

	if len(entries) != 1 || entries[0].IsDir() {
		return "", "", fmt.Errorf("k3s install path contains unexpected entries: %v", entries)
	}

	binaryPath = filepath.Join(installPath, entries[0].Name())
	return prependArtefactPath(binaryPath), prependArtefactPath(imagesPath), nil
}

func (c *Combustion) configureRKE2(ctx *image.Context, cluster *kubernetes.Cluster) (string, error) {
	zap.S().Info("Configuring RKE2 cluster")

	installScript, err := c.downloadKubernetesInstallScript(ctx, image.KubernetesDistroRKE2)
	if err != nil {
		return "", fmt.Errorf("downloading RKE2 install script: %w", err)
	}

	installPath, imagesPath, err := c.downloadRKE2Artefacts(ctx, cluster)
	if err != nil {
		return "", fmt.Errorf("downloading RKE2 artefacts: %w", err)
	}

	manifestsPath, err := c.configureManifests(ctx)
	if err != nil {
		return "", fmt.Errorf("configuring kubernetes manifests: %w", err)
	}

	templateValues := map[string]any{
		"installScript":   installScript,
		"apiVIP":          ctx.ImageDefinition.Kubernetes.Network.APIVIP,
		"apiHost":         ctx.ImageDefinition.Kubernetes.Network.APIHost,
		"installPath":     installPath,
		"imagesPath":      imagesPath,
		"manifestsPath":   manifestsPath,
		"configFilePath":  prependArtefactPath(k8sDir),
		"registryMirrors": prependArtefactPath(filepath.Join(k8sDir, registryMirrorsFileName)),
	}

	singleNode := len(ctx.ImageDefinition.Kubernetes.Nodes) < 2
	if singleNode {
		if ctx.ImageDefinition.Kubernetes.Network.APIVIP == "" {
			zap.S().Info("Virtual IP address for RKE2 cluster is not provided and will not be configured")
		}

		templateValues["configFile"] = k8sServerConfigFile

		return storeKubernetesInstaller(ctx, "single-node-rke2", rke2SingleNodeInstaller, templateValues)
	}

	templateValues["nodes"] = ctx.ImageDefinition.Kubernetes.Nodes
	templateValues["initialiser"] = cluster.InitialiserName
	templateValues["initialiserConfigFile"] = k8sInitServerConfigFile

	return storeKubernetesInstaller(ctx, "multi-node-rke2", rke2MultiNodeInstaller, templateValues)
}

func storeKubernetesInstaller(ctx *image.Context, templateName, templateContents string, templateValues any) (string, error) {
	data, err := template.Parse(templateName, templateContents, templateValues)
	if err != nil {
		return "", fmt.Errorf("parsing '%s' template: %w", templateName, err)
	}

	installScript := filepath.Join(ctx.CombustionDir, k8sInstallScript)
	if err = os.WriteFile(installScript, []byte(data), fileio.ExecutablePerms); err != nil {
		return "", fmt.Errorf("writing kubernetes install script: %w", err)
	}

	return k8sInstallScript, nil
}

func (c *Combustion) downloadRKE2Artefacts(ctx *image.Context, cluster *kubernetes.Cluster) (installPath, imagesPath string, err error) {
	cni, multusEnabled, err := cluster.ExtractCNI()
	if err != nil {
		return "", "", fmt.Errorf("extracting CNI from cluster config: %w", err)
	}

	imagesPath = filepath.Join(k8sDir, k8sImagesDir)
	imagesDestination := filepath.Join(ctx.ArtefactsDir, imagesPath)
	if err = os.MkdirAll(imagesDestination, os.ModePerm); err != nil {
		return "", "", fmt.Errorf("creating kubernetes images dir: %w", err)
	}

	installPath = filepath.Join(k8sDir, k8sInstallDir)
	installDestination := filepath.Join(ctx.ArtefactsDir, installPath)
	if err = os.MkdirAll(installDestination, os.ModePerm); err != nil {
		return "", "", fmt.Errorf("creating kubernetes install dir: %w", err)
	}

	if err = c.KubernetesArtefactDownloader.DownloadRKE2Artefacts(
		ctx.ImageDefinition.Image.Arch,
		ctx.ImageDefinition.Kubernetes.Version,
		cni,
		multusEnabled,
		installDestination,
		imagesDestination,
	); err != nil {
		return "", "", fmt.Errorf("downloading artefacts: %w", err)
	}

	return prependArtefactPath(installPath), prependArtefactPath(imagesPath), nil
}

func kubernetesVIPManifest(k *image.Kubernetes) (string, error) {
	manifest := struct {
		APIAddress string
		RKE2       bool
	}{
		APIAddress: k.Network.APIVIP,
		RKE2:       strings.Contains(k.Version, image.KubernetesDistroRKE2),
	}

	return template.Parse("k8s-vip", k8sVIPManifest, &manifest)
}

func storeKubernetesClusterConfig(cluster *kubernetes.Cluster, destPath string) error {
	serverConfig := filepath.Join(destPath, k8sServerConfigFile)
	if err := storeKubernetesConfig(cluster.ServerConfig, serverConfig); err != nil {
		return fmt.Errorf("storing server config file: %w", err)
	}

	if cluster.InitialiserConfig != nil {
		initialiserConfig := filepath.Join(destPath, k8sInitServerConfigFile)

		if err := storeKubernetesConfig(cluster.InitialiserConfig, initialiserConfig); err != nil {
			return fmt.Errorf("storing init server config file: %w", err)
		}
	}

	if cluster.AgentConfig != nil {
		agentConfig := filepath.Join(destPath, k8sAgentConfigFile)

		if err := storeKubernetesConfig(cluster.AgentConfig, agentConfig); err != nil {
			return fmt.Errorf("storing agent config file: %w", err)
		}
	}

	return nil
}

func storeKubernetesConfig(config map[string]any, configPath string) error {
	data, err := yaml.Marshal(config)
	if err != nil {
		return fmt.Errorf("serializing kubernetes config: %w", err)
	}

	return os.WriteFile(configPath, data, fileio.NonExecutablePerms)
}

func (c *Combustion) configureManifests(ctx *image.Context) (string, error) {
	var manifestsPathPopulated bool

	manifestsPath := localKubernetesManifestsPath()
	manifestDestDir := filepath.Join(ctx.ArtefactsDir, manifestsPath)

	if ctx.ImageDefinition.Kubernetes.Network.APIVIP != "" {
		if err := os.MkdirAll(manifestDestDir, os.ModePerm); err != nil {
			return "", fmt.Errorf("creating manifests destination dir: %w", err)
		}

		manifest, err := kubernetesVIPManifest(&ctx.ImageDefinition.Kubernetes)
		if err != nil {
			return "", fmt.Errorf("parsing VIP manifest: %w", err)
		}

		manifestPath := filepath.Join(manifestDestDir, "k8s-vip.yaml")
		if err = os.WriteFile(manifestPath, []byte(manifest), fileio.NonExecutablePerms); err != nil {
			return "", fmt.Errorf("storing VIP manifest: %w", err)
		}

		manifestsPathPopulated = true
	}

	if c.Registry != nil {
		if c.Registry.ManifestsPath() != "" {
			if err := fileio.CopyFiles(c.Registry.ManifestsPath(), manifestDestDir, "", false); err != nil {
				return "", fmt.Errorf("copying manifests to combustion dir: %w", err)
			}

			manifestsPathPopulated = true
		}

		charts, err := c.Registry.HelmCharts()
		if err != nil {
			return "", fmt.Errorf("getting helm charts: %w", err)
		}

		if len(charts) != 0 {
			if err = os.MkdirAll(manifestDestDir, os.ModePerm); err != nil {
				return "", fmt.Errorf("creating manifests destination dir: %w", err)
			}

			for _, chart := range charts {
				data, err := yaml.Marshal(chart)
				if err != nil {
					return "", fmt.Errorf("marshaling helm chart: %w", err)
				}

				chartFileName := fmt.Sprintf("%s.yaml", chart.Metadata.Name)
				if err = os.WriteFile(filepath.Join(manifestDestDir, chartFileName), data, fileio.NonExecutablePerms); err != nil {
					return "", fmt.Errorf("storing helm chart: %w", err)
				}
			}

			manifestsPathPopulated = true
		}
	}

	if !manifestsPathPopulated {
		return "", nil
	}

	return prependArtefactPath(manifestsPath), nil
}

func KubernetesConfigPath(ctx *image.Context) string {
	return filepath.Join(ctx.ImageConfigDir, k8sDir, k8sConfigDir, k8sServerConfigFile)
}

func localKubernetesManifestsPath() string {
	return filepath.Join(k8sDir, k8sManifestsDir)
}

func KubernetesManifestsPath(ctx *image.Context) string {
	return filepath.Join(ctx.ImageConfigDir, localKubernetesManifestsPath())
}

func HelmValuesPath(ctx *image.Context) string {
	return filepath.Join(ctx.ImageConfigDir, k8sDir, helmDir, helmValuesDir)
}

func HelmCertsPath(ctx *image.Context) string {
	return filepath.Join(ctx.ImageConfigDir, k8sDir, helmDir, helmCertsDir)
}

func kubernetesArtefactsPath(ctx *image.Context) string {
	return filepath.Join(ctx.ArtefactsDir, k8sDir)
}
