package main

import (
	"encoding/json"
	"encoding/xml"
	"flag"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strconv"
	"time"
)

// --- Config types ---

type ServerVersion struct {
	Version string `json:"version"`
	Weight  int    `json:"weight"`
}

type Platform struct {
	Name         string   `json:"Platform"`
	Enabled      bool     `json:"enabled"`
	Versions     []string `json:"versions"`
	StorageClass string   `json:"storageClass"`
	PlatformType string   `json:"platformType"`
}

type VersionEntry struct {
	VersionBranch           string              `json:"VersionBranch"`
	Refspec                 string              `json:"refspec"`
	SupportedServerVersions []ServerVersion     `json:"supportedServerVersions"`
	UpgradePaths            map[string][]string `json:"upgradePaths"`
	SupportedPlatforms      []Platform          `json:"supportedPlatforms"`
	Enabled                 bool                `json:"enabled"`
}

type MatrixConfig struct {
	Versions []VersionEntry `json:"Versions"`
}

// --- Output type ---

type MatrixOutput struct {
	Refspec                 string `json:"refspec"`
	VersionBranch           string `json:"version_branch"`
	Platform                string `json:"platform"`
	PlatformType            string `json:"platform_type"`
	KubernetesVersion       string `json:"kubernetes_version"`
	KubectlVersion          string `json:"kubectl_version"`
	ServerImage             string `json:"server_image"`
	ServerImageUpgrade      string `json:"server_image_upgrade"`
	OperatorImage           string `json:"operator_image"`
	AdmissionImage          string `json:"admission_image"`
	CertificationImage      string `json:"certification_image"`
	BackupImage             string `json:"backup_image"`
	ExporterImage           string `json:"exporter_image"`
	ExporterImageUpgrade    string `json:"exporter_image_upgrade"`
	LoggingImage            string `json:"logging_image"`
	LoggingImageUpgrade     string `json:"logging_image_upgrade"`
	CloudNativeGatewayImage string `json:"cloud_native_gateway_image"`
	MobileImage             string `json:"mobile_image"`
	StorageClass            string `json:"storage_class"`
}

// --- Manifest XML types ---

type ManifestAnnotation struct {
	Name  string `xml:"name,attr"`
	Value string `xml:"value,attr"`
}

type ManifestProject struct {
	Annotations []ManifestAnnotation `xml:"annotation"`
}

type Manifest struct {
	Default  struct{}          `xml:"default"`
	Projects []ManifestProject `xml:"project"`
}

// --- GHCR types ---

type GHCRToken struct {
	Token string `json:"token"`
}

type GHCRTagEntry struct {
	Name string `json:"name"`
}

type GHCRTagList struct {
	Tags []string `json:"tags"`
}

func main() {
	configPath := flag.String("config", "matrix-config.json", "Path to matrix config JSON")
	dateOverride := flag.String("date", "", "Override date for testing (YYYY-MM-DD)")
	skipManifest := flag.Bool("skip-manifest", false, "Skip manifest lookup, use latest tag for operator")
	ghcrUser := flag.String("ghcr-user", "", "GHCR username for authenticated tag listing")
	ghcrPass := flag.String("ghcr-pass", "", "GHCR password/PAT for authenticated tag listing")
	branchOverride := flag.String("branch", "", "Override branch selection (e.g. master, 2.9.x)")
	listBranches := flag.Bool("list-branches", false, "Print enabled branch names as JSON array and exit")
	flag.Parse()

	if *ghcrUser == "" {
		*ghcrUser = os.Getenv("GHCR_USER")
	}
	if *ghcrPass == "" {
		*ghcrPass = os.Getenv("GHCR_PASS")
	}

	data, err := os.ReadFile(*configPath)
	if err != nil {
		log.Fatalf("Failed to read config: %v", err)
	}

	var config MatrixConfig
	if err := json.Unmarshal(data, &config); err != nil {
		log.Fatalf("Failed to parse config: %v", err)
	}

	// List enabled branches mode -- used by Jenkinsfile to iterate.
	if *listBranches {
		var names []string
		for _, v := range config.Versions {
			if v.Enabled {
				names = append(names, v.VersionBranch)
			}
		}
		out, _ := json.Marshal(names)
		fmt.Println(string(out))
		return
	}

	now := time.Now()
	if *dateOverride != "" {
		parsed, err := time.Parse("2006-01-02", *dateOverride)
		if err != nil {
			log.Fatalf("Invalid date format, use YYYY-MM-DD: %v", err)
		}
		now = parsed
	}

	output := generateMatrixForBranch(config, now, *skipManifest, *ghcrUser, *ghcrPass, *branchOverride)

	jsonOut, err := json.MarshalIndent(output, "", "  ")
	if err != nil {
		log.Fatalf("Failed to marshal output: %v", err)
	}

	fmt.Println(string(jsonOut))
}

// generateMatrix picks a single branch via round-robin (legacy behavior for tests).
func generateMatrix(config MatrixConfig, now time.Time, skipManifest bool, ghcrToken string) MatrixOutput {
	return generateMatrixForBranch(config, now, skipManifest, "", ghcrToken, "")
}

func generateMatrixForBranch(config MatrixConfig, now time.Time, skipManifest bool, ghcrUser string, ghcrPass string, branchOverride string) MatrixOutput {
	enabledBranches := getEnabledBranches(config)
	if len(enabledBranches) == 0 {
		log.Fatal("No enabled version branches found")
	}

	dayOfYear := now.YearDay()
	dayOfMonth := now.Day()
	weekOfYear := (dayOfYear-1)/7 + 1

	// Step 1: Pick operator branch
	var branch VersionEntry
	if branchOverride != "" {
		found := false
		for _, b := range enabledBranches {
			if b.VersionBranch == branchOverride {
				branch = b
				found = true
				break
			}
		}
		if !found {
			log.Fatalf("Branch %q not found or not enabled", branchOverride)
		}
		log.Printf("Selected branch: %s (override)", branch.VersionBranch)
	} else {
		branch = enabledBranches[dayOfYear%len(enabledBranches)]
		log.Printf("Selected branch: %s (day_of_year=%d, index=%d)", branch.VersionBranch, dayOfYear, dayOfYear%len(enabledBranches))
	}

	// Step 2: Pick platform (round-robin by day of month)
	enabledPlatforms := getEnabledPlatforms(branch)
	if len(enabledPlatforms) == 0 {
		log.Fatal("No enabled platforms found for branch: " + branch.VersionBranch)
	}
	platform := enabledPlatforms[dayOfMonth%len(enabledPlatforms)]
	log.Printf("Selected platform: %s (day_of_month=%d, index=%d)", platform.Name, dayOfMonth, dayOfMonth%len(enabledPlatforms))

	// Step 3: Pick K8s/OpenShift version (round-robin by week of year)
	k8sVersion := platform.Versions[weekOfYear%len(platform.Versions)]
	log.Printf("Selected k8s version: %s (week_of_year=%d, index=%d)", k8sVersion, weekOfYear, weekOfYear%len(platform.Versions))

	// Step 4: Pick server version (weighted random)
	serverVersion := selectWeightedServer(branch.SupportedServerVersions, now)
	log.Printf("Selected server version: %s", serverVersion)

	// Step 5: Pick upgrade path
	upgradeVersion := selectUpgradeVersion(branch.UpgradePaths, serverVersion, now)
	log.Printf("Selected upgrade version: %s", upgradeVersion)

	// Step 6: Resolve operator image from manifest
	operatorTag := resolveOperatorTag(branch.VersionBranch, platform, skipManifest, ghcrUser, ghcrPass)

	// Step 7: Build all images
	serverImage := resolveServerImage(serverVersion, platform)
	upgradeImage := resolveServerImage(upgradeVersion, platform)
	operatorImage, admissionImage, certImage, backupImage, loggingImage, cngImage, mobileImage := resolveSidecarImages(platform, operatorTag)

	kubectlVersion := resolveKubectlVersion(k8sVersion, platform)

	return MatrixOutput{
		Refspec:                 branch.Refspec,
		VersionBranch:           branch.VersionBranch,
		Platform:                platform.Name,
		PlatformType:            platform.PlatformType,
		KubernetesVersion:       k8sVersion,
		KubectlVersion:          kubectlVersion,
		ServerImage:             serverImage,
		ServerImageUpgrade:      upgradeImage,
		OperatorImage:           operatorImage,
		AdmissionImage:          admissionImage,
		CertificationImage:      certImage,
		BackupImage:             backupImage,
		ExporterImage:           "couchbase/exporter:1.0.10",
		ExporterImageUpgrade:    "couchbase/exporter:1.0.5",
		LoggingImage:            loggingImage,
		LoggingImageUpgrade:     "couchbase/fluent-bit:1.1.1",
		CloudNativeGatewayImage: cngImage,
		MobileImage:             mobileImage,
		StorageClass:            platform.StorageClass,
	}
}

func getEnabledBranches(config MatrixConfig) []VersionEntry {
	var enabled []VersionEntry
	for _, v := range config.Versions {
		if v.Enabled {
			enabled = append(enabled, v)
		}
	}
	return enabled
}

func getEnabledPlatforms(branch VersionEntry) []Platform {
	var enabled []Platform
	for _, p := range branch.SupportedPlatforms {
		if p.Enabled {
			enabled = append(enabled, p)
		}
	}
	return enabled
}

func selectWeightedServer(versions []ServerVersion, now time.Time) string {
	if len(versions) == 0 {
		log.Fatal("No server versions configured")
	}

	totalWeight := 0
	for _, v := range versions {
		totalWeight += v.Weight
	}

	rng := rand.New(rand.NewSource(int64(now.Year()*1000 + now.YearDay())))
	roll := rng.Intn(totalWeight)

	cumulative := 0
	for _, v := range versions {
		cumulative += v.Weight
		if roll < cumulative {
			return v.Version
		}
	}

	return versions[len(versions)-1].Version
}

func selectUpgradeVersion(upgradePaths map[string][]string, serverVersion string, now time.Time) string {
	paths, ok := upgradePaths[serverVersion]
	if !ok || len(paths) == 0 {
		log.Printf("No upgrade paths for server %s, using same version", serverVersion)
		return serverVersion
	}

	rng := rand.New(rand.NewSource(int64(now.Year()*1000 + now.YearDay() + 7)))
	return paths[rng.Intn(len(paths))]
}

// resolveOperatorTag fetches the manifest XML for the branch, extracts the base
// version, then queries GHCR for the latest build tag matching that base version.
// Falls back to "latest" on any error.
func resolveOperatorTag(branch string, platform Platform, skipManifest bool, ghcrUser string, ghcrPass string) string {
	if skipManifest {
		log.Printf("Manifest lookup skipped, using 'latest' tag")
		return "latest"
	}

	registry := "cb-vanilla"
	if platform.PlatformType == "openshift" {
		registry = "cb-rhcc"
	}

	baseVersion, err := fetchManifestVersion(branch)
	if err != nil {
		log.Printf("WARNING: Failed to fetch manifest version: %v. Falling back to 'latest'", err)
		return "latest"
	}
	log.Printf("Manifest base version for %s: %s", branch, baseVersion)

	latestTag, err := findLatestGHCRBuild(registry, "operator", baseVersion, ghcrUser, ghcrPass)
	if err != nil {
		log.Printf("WARNING: Failed to find latest GHCR build: %v. Falling back to 'latest'", err)
		return "latest"
	}
	log.Printf("Resolved operator tag: %s", latestTag)

	return latestTag
}

// fetchManifestVersion fetches the manifest XML from GitHub and extracts the VERSION annotation.
func fetchManifestVersion(branch string) (string, error) {
	url := fmt.Sprintf("https://raw.githubusercontent.com/couchbase/manifest/master/couchbase-operator/%s.xml", branch)

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return "", fmt.Errorf("fetch manifest: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("manifest returned HTTP %d for branch %s", resp.StatusCode, branch)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read manifest body: %w", err)
	}

	// Parse XML for VERSION annotation
	re := regexp.MustCompile(`<annotation\s+name="VERSION"\s+value="([^"]+)"`)
	matches := re.FindSubmatch(body)
	if len(matches) < 2 {
		// Try XML parsing as fallback
		var manifest Manifest
		if xmlErr := xml.Unmarshal(body, &manifest); xmlErr == nil {
			for _, proj := range manifest.Projects {
				for _, ann := range proj.Annotations {
					if ann.Name == "VERSION" {
						return ann.Value, nil
					}
				}
			}
		}
		return "", fmt.Errorf("VERSION annotation not found in manifest %s.xml", branch)
	}

	return string(matches[1]), nil
}

// findLatestGHCRBuild queries the GHCR registry for tags matching baseVersion-NNN
// and returns the one with the highest build number.
func findLatestGHCRBuild(registry, image, baseVersion, ghcrUser, ghcrPass string) (string, error) {
	client := &http.Client{Timeout: 10 * time.Second}

	// Get bearer token from GHCR token endpoint, with optional Basic auth for private packages.
	tokenURL := fmt.Sprintf("https://ghcr.io/token?scope=repository:%s/%s:pull", registry, image)
	tokenReq, _ := http.NewRequest("GET", tokenURL, nil)
	if ghcrUser != "" && ghcrPass != "" {
		tokenReq.SetBasicAuth(ghcrUser, ghcrPass)
	}

	tokenResp, err := client.Do(tokenReq)
	if err != nil {
		return "", fmt.Errorf("fetch GHCR token: %w", err)
	}
	defer tokenResp.Body.Close()

	if tokenResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(tokenResp.Body)
		return "", fmt.Errorf("GHCR token request returned HTTP %d: %s", tokenResp.StatusCode, string(body))
	}

	var tok GHCRToken
	if err := json.NewDecoder(tokenResp.Body).Decode(&tok); err != nil {
		return "", fmt.Errorf("decode GHCR token: %w", err)
	}
	log.Printf("GHCR token obtained (length=%d)", len(tok.Token))

	// List all tags with pagination
	pattern := regexp.MustCompile(`^` + regexp.QuoteMeta(baseVersion) + `-(\d+)$`)
	type buildTag struct {
		tag    string
		number int
	}
	var matches []buildTag
	totalTags := 0

	nextURL := fmt.Sprintf("https://ghcr.io/v2/%s/%s/tags/list?n=1000", registry, image)
	for nextURL != "" {
		req, _ := http.NewRequest("GET", nextURL, nil)
		req.Header.Set("Authorization", "Bearer "+tok.Token)

		resp, err := client.Do(req)
		if err != nil {
			return "", fmt.Errorf("list GHCR tags: %w", err)
		}

		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			return "", fmt.Errorf("GHCR tags list returned HTTP %d: %s", resp.StatusCode, string(body))
		}

		var tagList GHCRTagList
		if err = json.NewDecoder(resp.Body).Decode(&tagList); err != nil {
			resp.Body.Close()
			return "", fmt.Errorf("decode GHCR tags: %w", err)
		}
		resp.Body.Close()

		totalTags += len(tagList.Tags)
		for _, tag := range tagList.Tags {
			m := pattern.FindStringSubmatch(tag)
			if m != nil {
				num, _ := strconv.Atoi(m[1])
				matches = append(matches, buildTag{tag: tag, number: num})
			}
		}

		// Follow pagination via Link header
		nextURL = ""
		linkHeader := resp.Header.Get("Link")
		if linkHeader != "" {
			re := regexp.MustCompile(`<([^>]+)>;\s*rel="next"`)
			if lm := re.FindStringSubmatch(linkHeader); lm != nil {
				parsed := lm[1]
				// GHCR returns relative URLs in Link header; prepend base URL.
				if len(parsed) > 0 && parsed[0] == '/' {
					parsed = "https://ghcr.io" + parsed
				}
				nextURL = parsed
			}
		}
	}

	log.Printf("GHCR returned %d total tags for %s/%s, %d matched %s-NNN", totalTags, registry, image, len(matches), baseVersion)

	if len(matches) == 0 {
		return "", fmt.Errorf("no tags matching %s-NNN found in %s/%s (%d total tags scanned)", baseVersion, registry, image, totalTags)
	}

	sort.Slice(matches, func(i, j int) bool {
		return matches[i].number > matches[j].number
	})

	return matches[0].tag, nil
}

// resolveKubectlVersion returns a kubectl version matching the selected K8s version.
// For OpenShift, kubectl is not used (oc client is used), so we return an empty string.
// For K8s platforms, we use the minor version with the latest known patch.
func resolveKubectlVersion(k8sVersion string, platform Platform) string {
	if platform.PlatformType == "openshift" {
		return ""
	}
	// kubectl should match the cluster minor version.
	// Use <k8sVersion>.0 as a safe default patch level if the version has no patch.
	parts := 0
	for _, c := range k8sVersion {
		if c == '.' {
			parts++
		}
	}
	if parts == 1 {
		// e.g. "1.31" -> "1.31.0"
		return k8sVersion + ".0"
	}
	// Already has patch, e.g. "1.31.1"
	return k8sVersion
}

func resolveServerImage(version string, platform Platform) string {
	if platform.PlatformType == "openshift" {
		return fmt.Sprintf("registry.connect.redhat.com/couchbase/server:%s", version)
	}
	return fmt.Sprintf("ghcr.io/cb-vanilla/server:%s", version)
}

func resolveSidecarImages(platform Platform, operatorTag string) (operator, admission, cert, backup, logging, cng, mobile string) {
	if platform.PlatformType == "openshift" {
		operator = fmt.Sprintf("ghcr.io/cb-rhcc/operator:%s", operatorTag)
		admission = "ghcr.io/cb-rhcc/admission-controller:latest"
		cert = "ghcr.io/cb-rhcc/operator-certification:latest"
		backup = "ghcr.io/cb-rhcc/operator-backup:latest"
		logging = "ghcr.io/cb-rhcc/fluent-bit:latest"
		cng = "ghcr.io/cb-rhcc/cloud-native-gateway:latest"
		mobile = "ghcr.io/cb-rhcc/sync-gateway:latest"
	} else {
		operator = fmt.Sprintf("ghcr.io/cb-vanilla/operator:%s", operatorTag)
		admission = "ghcr.io/cb-vanilla/admission-controller:latest"
		cert = "ghcr.io/cb-vanilla/operator-certification:latest"
		backup = "ghcr.io/cb-vanilla/operator-backup:latest"
		logging = "ghcr.io/cb-vanilla/fluent-bit:latest"
		cng = "ghcr.io/cb-vanilla/cloud-native-gateway:latest"
		mobile = "ghcr.io/cb-vanilla/sync-gateway:4.0.0-enterprise"
	}
	return
}
