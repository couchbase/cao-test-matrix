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
	"strings"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/crane"
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
	// SHAImageWeight is how many runs out of 100 name the server image by
	// digest instead of by tag. Leave it out to use defaultSHAWeight.
	SHAImageWeight *int `json:"shaImageWeight,omitempty"`
}

type MatrixOutput struct {
	Refspec            string `json:"refspec"`
	VersionBranch      string `json:"version_branch"`
	Platform           string `json:"platform"`
	PlatformType       string `json:"platform_type"`
	KubernetesVersion  string `json:"kubernetes_version"`
	KubectlVersion     string `json:"kubectl_version"`
	ServerImage        string `json:"server_image"`
	ServerImageUpgrade string `json:"server_image_upgrade"`
	// The plain versions are always sent, even when the images above are
	// digests. A digest does not say which version it is, and the test
	// framework only knows the digests of released builds, so without these
	// it falls back to a placeholder version and version checks go wrong.
	ServerImageVersion        string `json:"server_image_version"`
	ServerImageUpgradeVersion string `json:"server_image_upgrade_version"`
	OperatorImage             string `json:"operator_image"`
	AdmissionImage            string `json:"admission_image"`
	CertificationImage        string `json:"certification_image"`
	BackupImage               string `json:"backup_image"`
	ExporterImage             string `json:"exporter_image"`
	ExporterImageUpgrade      string `json:"exporter_image_upgrade"`
	LoggingImage              string `json:"logging_image"`
	LoggingImageUpgrade       string `json:"logging_image_upgrade"`
	CloudNativeGatewayImage   string `json:"cloud_native_gateway_image"`
	MobileImage               string `json:"mobile_image"`
	StorageClass              string `json:"storage_class"`
}

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
	shaWeightOverride := flag.Int("sha-weight", -1, "Override percentage of runs using digest refs instead of tags (0-100, -1 uses config)")
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

	if config.SHAImageWeight != nil && !validSHAWeight(*config.SHAImageWeight) {
		log.Fatalf("Invalid shaImageWeight %d in config, must be 0-100", *config.SHAImageWeight)
	}

	// -1 means "not set", anything else has to be a real percentage.
	if *shaWeightOverride != -1 {
		if !validSHAWeight(*shaWeightOverride) {
			log.Fatalf("Invalid -sha-weight %d, must be 0-100", *shaWeightOverride)
		}
		config.SHAImageWeight = shaWeightOverride
	}

	// List enabled branches mode, used by Jenkinsfile to iterate.
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

	// Step 7b: Optionally reference the server images by digest instead of tag.
	// skipManifest doubles as offline mode, so tests do not hit the registry.
	if !skipManifest {
		w := shaWeight(config)
		serverImage = maybeSHAImage(serverImage, w, now, shaOffsetServer, ghcrUser, ghcrPass, "server_image")
		upgradeImage = maybeSHAImage(upgradeImage, w, now, shaOffsetUpgrade, ghcrUser, ghcrPass, "server_image_upgrade")
	}
	operatorImage, admissionImage, certImage, backupImage, loggingImage, cngImage, mobileImage := resolveSidecarImages(platform, operatorTag)

	kubectlVersion := resolveKubectlVersion(k8sVersion, platform)

	return MatrixOutput{
		Refspec:                   branch.Refspec,
		VersionBranch:             branch.VersionBranch,
		Platform:                  platform.Name,
		PlatformType:              platform.PlatformType,
		KubernetesVersion:         k8sVersion,
		KubectlVersion:            kubectlVersion,
		ServerImage:               serverImage,
		ServerImageUpgrade:        upgradeImage,
		ServerImageVersion:        serverVersion,
		ServerImageUpgradeVersion: upgradeVersion,
		OperatorImage:             operatorImage,
		AdmissionImage:            admissionImage,
		CertificationImage:        certImage,
		BackupImage:               backupImage,
		ExporterImage:             "couchbase/exporter:1.0.10",
		ExporterImageUpgrade:      "couchbase/exporter:1.0.5",
		LoggingImage:              loggingImage,
		LoggingImageUpgrade:       "couchbase/fluent-bit:1.1.1",
		CloudNativeGatewayImage:   cngImage,
		MobileImage:               mobileImage,
		StorageClass:              platform.StorageClass,
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

// defaultSHAWeight is how many runs out of 100 use a digest instead of a tag
// when the config does not say.
const defaultSHAWeight = 25

// ghcrPrefix is the only registry we hold credentials for, so it is the only
// one where a digest lookup can succeed.
const ghcrPrefix = "ghcr.io/"

// Each image adds a different number to the random seed, so the server image
// and the upgrade image are decided separately. Over time that gives all four
// mixes: tag->tag, tag->sha, sha->tag and sha->sha.
const (
	shaOffsetServer  = 101
	shaOffsetUpgrade = 202
)

// validSHAWeight reports whether w is a usable percentage.
func validSHAWeight(w int) bool {
	return w >= 0 && w <= 100
}

func shaWeight(config MatrixConfig) int {
	if config.SHAImageWeight == nil {
		return defaultSHAWeight
	}
	return *config.SHAImageWeight
}

// shouldUseSHA decides whether to use a digest instead of a tag. The answer
// comes from the date, so running the same day twice gives the same answer.
func shouldUseSHA(weight int, now time.Time, offset int64) bool {
	if weight <= 0 {
		return false
	}
	if weight >= 100 {
		return true
	}
	rng := rand.New(rand.NewSource(int64(now.Year()*1000+now.YearDay()) + offset))
	return rng.Intn(100) < weight
}

// toDigestRef turns repo:tag into repo@sha256:... The last colon is only a tag
// if it comes after the last slash. If not, it is a port, like localhost:5000.
func toDigestRef(image, digest string) string {
	repo := image
	if i := strings.LastIndex(image, ":"); i > strings.LastIndex(image, "/") {
		repo = image[:i]
	}
	return repo + "@" + digest
}

// resolveImageDigest asks the registry which image the tag points at right now.
func resolveImageDigest(image, ghcrUser, ghcrPass string) (string, error) {
	var opts []crane.Option
	if strings.HasPrefix(image, ghcrPrefix) && ghcrUser != "" && ghcrPass != "" {
		opts = append(opts, crane.WithAuth(&authn.Basic{Username: ghcrUser, Password: ghcrPass}))
	}
	return crane.Digest(image, opts...)
}

// maybeSHAImage turns a tag into a digest when the roll picks SHA. We only
// look up GHCR images, since that is the one registry we have a login for.
// OpenShift pulls its server image from registry.connect.redhat.com, so it
// keeps its tag. If a lookup fails we keep the tag as well.
func maybeSHAImage(image string, weight int, now time.Time, offset int64, ghcrUser, ghcrPass, label string) string {
	if !shouldUseSHA(weight, now, offset) {
		log.Printf("%s: using tag %s", label, image)
		return image
	}

	// Checked after the roll so this only shows up when a digest was wanted.
	if !strings.HasPrefix(image, ghcrPrefix) {
		log.Printf("%s: digests are not available for this registry, using tag %s", label, image)
		return image
	}

	digest, err := resolveImageDigest(image, ghcrUser, ghcrPass)
	if err != nil {
		log.Printf("WARNING: %s: could not resolve digest for %s: %v. Falling back to tag", label, image, err)
		return image
	}

	ref := toDigestRef(image, digest)
	log.Printf("%s: using digest %s", label, ref)
	return ref
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
		cert = fmt.Sprintf("ghcr.io/cb-rhcc/operator-certification:%s", operatorTag)
		backup = "ghcr.io/cb-rhcc/operator-backup:latest"
		logging = "ghcr.io/cb-rhcc/fluent-bit:latest"
		cng = "ghcr.io/cb-rhcc/cloud-native-gateway:latest"
		mobile = "ghcr.io/cb-rhcc/sync-gateway:latest"
	} else {
		operator = fmt.Sprintf("ghcr.io/cb-vanilla/operator:%s", operatorTag)
		admission = "ghcr.io/cb-vanilla/admission-controller:latest"
		cert = fmt.Sprintf("ghcr.io/cb-vanilla/operator-certification:%s", operatorTag)
		backup = "ghcr.io/cb-vanilla/operator-backup:latest"
		logging = "ghcr.io/cb-vanilla/fluent-bit:latest"
		cng = "ghcr.io/cb-vanilla/cloud-native-gateway:latest"
		mobile = "ghcr.io/cb-vanilla/sync-gateway:4.0.0-enterprise"
	}
	return
}
