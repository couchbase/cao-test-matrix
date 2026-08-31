package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

func loadTestConfig(t *testing.T) MatrixConfig {
	t.Helper()
	data, err := os.ReadFile("matrix-config.json")
	if err != nil {
		t.Fatalf("Failed to read matrix-config.json: %v", err)
	}
	var config MatrixConfig
	if err := json.Unmarshal(data, &config); err != nil {
		t.Fatalf("Failed to parse config: %v", err)
	}
	return config
}

func TestGetEnabledBranches(t *testing.T) {
	config := loadTestConfig(t)
	enabled := getEnabledBranches(config)

	if len(enabled) < 1 {
		t.Fatal("Expected at least one enabled branch")
	}

	for _, b := range enabled {
		if !b.Enabled {
			t.Errorf("Branch %s should be enabled", b.VersionBranch)
		}
	}

	// 2.8.x should not appear
	for _, b := range enabled {
		if b.VersionBranch == "2.8.x" {
			t.Error("2.8.x is disabled and should not be in enabled list")
		}
	}
}

func TestGetEnabledPlatforms(t *testing.T) {
	config := loadTestConfig(t)
	enabled := getEnabledBranches(config)

	for _, branch := range enabled {
		platforms := getEnabledPlatforms(branch)
		if len(platforms) == 0 {
			t.Errorf("Branch %s has no enabled platforms", branch.VersionBranch)
		}
		for _, p := range platforms {
			if !p.Enabled {
				t.Errorf("Platform %s should be enabled", p.Name)
			}
			if len(p.Versions) == 0 {
				t.Errorf("Platform %s has no versions", p.Name)
			}
			if p.StorageClass == "" {
				t.Errorf("Platform %s has no storageClass", p.Name)
			}
			if p.PlatformType == "" {
				t.Errorf("Platform %s has no platformType", p.Name)
			}
		}
	}
}

func TestRoundRobinBranchSelection(t *testing.T) {
	config := loadTestConfig(t)
	enabled := getEnabledBranches(config)

	if len(enabled) < 2 {
		t.Skip("Need at least 2 enabled branches to test alternation")
	}

	day1 := time.Date(2026, 5, 8, 0, 0, 0, 0, time.UTC)
	day2 := time.Date(2026, 5, 9, 0, 0, 0, 0, time.UTC)

	branch1 := enabled[day1.YearDay()%len(enabled)]
	branch2 := enabled[day2.YearDay()%len(enabled)]

	if branch1.VersionBranch == branch2.VersionBranch {
		t.Errorf("Consecutive days selected the same branch: %s", branch1.VersionBranch)
	}
}

func TestRoundRobinPlatformSelection(t *testing.T) {
	config := loadTestConfig(t)
	enabled := getEnabledBranches(config)
	platforms := getEnabledPlatforms(enabled[0])

	// Different days of month should cycle platforms
	seen := make(map[string]bool)
	for day := 1; day <= 28; day++ {
		p := platforms[day%len(platforms)]
		seen[p.Name] = true
	}

	if len(seen) != len(platforms) {
		t.Errorf("Expected all %d platforms to be selected over 28 days, got %d", len(platforms), len(seen))
	}
}

func TestRoundRobinK8sVersionSelection(t *testing.T) {
	versions := []string{"1.33", "1.34", "1.35"}

	seen := make(map[string]bool)
	for week := 1; week <= 10; week++ {
		v := versions[week%len(versions)]
		seen[v] = true
	}

	if len(seen) != len(versions) {
		t.Errorf("Expected all %d versions to be selected over 10 weeks, got %d", len(versions), len(seen))
	}
}

func TestWeightedServerSelection(t *testing.T) {
	versions := []ServerVersion{
		{Version: "8.5.0", Weight: 5},
		{Version: "8.0.2", Weight: 4},
		{Version: "7.6.12", Weight: 3},
		{Version: "7.2.9", Weight: 2},
		{Version: "7.1.5", Weight: 1},
		{Version: "7.0.5", Weight: 1},
	}

	counts := make(map[string]int)
	// Run over 365 days to get a distribution
	for day := 1; day <= 365; day++ {
		date := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).AddDate(0, 0, day-1)
		v := selectWeightedServer(versions, date)
		counts[v]++
	}

	// Every version should be selected at least once
	for _, sv := range versions {
		if counts[sv.Version] == 0 {
			t.Errorf("Server version %s was never selected over 365 days", sv.Version)
		}
	}

	// 8.0.2 should be selected more than 7.0.5
	if counts["8.0.2"] <= counts["7.0.5"] {
		t.Errorf("8.0.2 (weight=4) should be selected more than 7.0.5 (weight=1): got %d vs %d",
			counts["8.0.2"], counts["7.0.5"])
	}

	t.Logf("Distribution over 365 days: %v", counts)
}

func TestUpgradePathSelection(t *testing.T) {
	upgradePaths := map[string][]string{
		"8.5.0":  {"8.0.2", "7.6.12"},
		"8.0.2":  {"7.6.12", "7.2.9"},
		"7.6.12": {"7.2.9", "7.1.5"},
		"7.2.9":  {"7.1.5", "7.0.5"},
		"7.1.5":  {"7.0.5"},
		"7.0.5":  {},
	}

	date := time.Date(2026, 5, 8, 0, 0, 0, 0, time.UTC)

	// 8.0.2 should upgrade from 7.6.12 or 7.2.9
	upgrade := selectUpgradeVersion(upgradePaths, "8.0.2", date)
	if upgrade != "7.6.12" && upgrade != "7.2.9" {
		t.Errorf("8.0.2 upgrade should be 7.6.12 or 7.2.9, got %s", upgrade)
	}

	// 7.0.5 has no upgrade path, should return itself
	upgrade = selectUpgradeVersion(upgradePaths, "7.0.5", date)
	if upgrade != "7.0.5" {
		t.Errorf("7.0.5 should return itself (no upgrade path), got %s", upgrade)
	}

	// All paths should only contain valid versions
	for server, paths := range upgradePaths {
		for _, path := range paths {
			if _, ok := upgradePaths[path]; !ok {
				t.Errorf("Upgrade path for %s references unknown version %s", server, path)
			}
		}
	}
}

func TestUpgradePathCoverage(t *testing.T) {
	upgradePaths := map[string][]string{
		"8.5.0":  {"8.0.2", "7.6.12"},
		"8.0.2":  {"7.6.12", "7.2.9"},
		"7.6.12": {"7.2.9", "7.1.5"},
	}

	// Over many days, both upgrade options should be selected
	for server, paths := range upgradePaths {
		if len(paths) < 2 {
			continue
		}
		seen := make(map[string]bool)
		for day := 1; day <= 365; day++ {
			date := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).AddDate(0, 0, day-1)
			v := selectUpgradeVersion(upgradePaths, server, date)
			seen[v] = true
		}
		if len(seen) != len(paths) {
			t.Errorf("Expected all %d upgrade paths for %s to be covered, got %d: %v",
				len(paths), server, len(seen), seen)
		}
	}
}

func TestServerImageResolution(t *testing.T) {
	k8sPlatform := Platform{PlatformType: "kubernetes"}
	ocPlatform := Platform{PlatformType: "openshift"}

	k8sImage := resolveServerImage("8.0.2", k8sPlatform)
	if k8sImage != "ghcr.io/cb-vanilla/server:8.0.2" {
		t.Errorf("Expected ghcr.io/cb-vanilla/server:8.0.2, got %s", k8sImage)
	}

	ocImage := resolveServerImage("7.6.12", ocPlatform)
	if ocImage != "registry.connect.redhat.com/couchbase/server:7.6.12" {
		t.Errorf("Expected registry.connect.redhat.com/couchbase/server:7.6.12, got %s", ocImage)
	}
}

func TestKubectlVersionResolution(t *testing.T) {
	tests := []struct {
		k8sVersion   string
		platformType string
		want         string
	}{
		{"1.35", "kubernetes", "1.35.0"},
		{"1.34", "kubernetes", "1.34.0"},
		{"1.33", "kubernetes", "1.33.0"},
		{"1.33.1", "kubernetes", "1.33.1"},
		{"4.20", "openshift", ""},
	}

	for _, tt := range tests {
		t.Run(tt.k8sVersion+"/"+tt.platformType, func(t *testing.T) {
			p := Platform{PlatformType: tt.platformType}
			got := resolveKubectlVersion(tt.k8sVersion, p)
			if got != tt.want {
				t.Errorf("resolveKubectlVersion(%q, %q) = %q, want %q", tt.k8sVersion, tt.platformType, got, tt.want)
			}
		})
	}
}

func TestBranchOverride(t *testing.T) {
	config := loadTestConfig(t)
	date := time.Date(2026, 5, 8, 0, 0, 0, 0, time.UTC)

	// Without override, day 128 picks master (index 0)
	out := generateMatrix(config, date, true, "")
	if out.VersionBranch != "master" {
		t.Errorf("Expected master without override, got %s", out.VersionBranch)
	}

	// With override to 2.9.x, should get 2.9.x regardless of date
	out = generateMatrixForBranch(config, date, true, "", "", "2.9.x")
	if out.VersionBranch != "2.9.x" {
		t.Errorf("Expected 2.9.x with override, got %s", out.VersionBranch)
	}
}

func TestListBranches(t *testing.T) {
	config := loadTestConfig(t)
	enabled := getEnabledBranches(config)

	if len(enabled) != 2 {
		t.Errorf("Expected 2 enabled branches (master, 2.9.x), got %d", len(enabled))
	}

	names := make(map[string]bool)
	for _, b := range enabled {
		names[b.VersionBranch] = true
	}

	if !names["master"] {
		t.Error("master branch should be enabled")
	}
	if !names["2.9.x"] {
		t.Error("2.9.x branch should be enabled")
	}
	if names["2.8.x"] {
		t.Error("2.8.x branch should NOT be enabled")
	}
}

func TestSidecarImageResolution(t *testing.T) {
	tests := []struct {
		name         string
		platformType string
		operatorTag  string
		wantRegistry string
	}{
		{"kubernetes with specific tag", "kubernetes", "2.10.0-45", "cb-vanilla"},
		{"kubernetes with latest", "kubernetes", "latest", "cb-vanilla"},
		{"openshift with specific tag", "openshift", "2.9.2-139", "cb-rhcc"},
		{"openshift with latest", "openshift", "latest", "cb-rhcc"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			platform := Platform{PlatformType: tt.platformType}
			operator, admission, cert, backup, logging, cng, mobile := resolveSidecarImages(platform, tt.operatorTag)

			expectedOperator := "ghcr.io/" + tt.wantRegistry + "/operator:" + tt.operatorTag
			if operator != expectedOperator {
				t.Errorf("operator = %s, want %s", operator, expectedOperator)
			}

			expectedCert := "ghcr.io/" + tt.wantRegistry + "/operator-certification:" + tt.operatorTag
			if cert != expectedCert {
				t.Errorf("cert = %s, want %s", cert, expectedCert)
			}

			for name, img := range map[string]string{
				"admission": admission, "backup": backup,
				"logging": logging, "cng": cng,
			} {
				if img == "" {
					t.Errorf("%s image is empty", name)
				}
				expected := "ghcr.io/" + tt.wantRegistry + "/"
				if len(img) < len(expected) || img[:len(expected)] != expected {
					t.Errorf("%s image %s doesn't use registry %s", name, img, tt.wantRegistry)
				}
			}

			if mobile == "" {
				t.Error("mobile image is empty")
			}
		})
	}
}

func TestDeterministicOutput(t *testing.T) {
	config := loadTestConfig(t)
	date := time.Date(2026, 6, 12, 0, 0, 0, 0, time.UTC)

	out1 := generateMatrix(config, date, true, "")
	out2 := generateMatrix(config, date, true, "")

	if out1 != out2 {
		t.Error("Same date should produce identical output")
	}
}

func TestDifferentDatesProduceDifferentOutput(t *testing.T) {
	config := loadTestConfig(t)
	date1 := time.Date(2026, 5, 8, 0, 0, 0, 0, time.UTC)
	date2 := time.Date(2026, 5, 9, 0, 0, 0, 0, time.UTC)

	out1 := generateMatrix(config, date1, true, "")
	out2 := generateMatrix(config, date2, true, "")

	// At minimum, branch should differ (consecutive days alternate with 2 branches)
	if out1.VersionBranch == out2.VersionBranch && out1.Platform == out2.Platform {
		t.Error("Consecutive days should produce different branch or platform selections")
	}
}

func TestConfigValidation(t *testing.T) {
	config := loadTestConfig(t)

	for _, version := range config.Versions {
		if version.VersionBranch == "" {
			t.Error("VersionBranch is empty")
		}
		if version.Refspec == "" {
			t.Errorf("Refspec is empty for branch %s", version.VersionBranch)
		}
		if len(version.SupportedServerVersions) == 0 {
			t.Errorf("No server versions for branch %s", version.VersionBranch)
		}

		// Every server version should have an upgrade path entry
		for _, sv := range version.SupportedServerVersions {
			if _, ok := version.UpgradePaths[sv.Version]; !ok {
				t.Errorf("Branch %s: server %s has no upgrade path entry", version.VersionBranch, sv.Version)
			}
			if sv.Weight <= 0 {
				t.Errorf("Branch %s: server %s has invalid weight %d", version.VersionBranch, sv.Version, sv.Weight)
			}
		}

		// Upgrade path targets should reference valid server versions
		validVersions := make(map[string]bool)
		for _, sv := range version.SupportedServerVersions {
			validVersions[sv.Version] = true
		}
		for server, paths := range version.UpgradePaths {
			if !validVersions[server] {
				t.Errorf("Branch %s: upgrade path key %s is not a supported server version", version.VersionBranch, server)
			}
			for _, target := range paths {
				if !validVersions[target] {
					t.Errorf("Branch %s: upgrade path %s -> %s references unknown version", version.VersionBranch, server, target)
				}
			}
		}

		if len(version.SupportedPlatforms) == 0 {
			t.Errorf("No platforms for branch %s", version.VersionBranch)
		}
	}
}

func TestSHAWeightDefault(t *testing.T) {
	if got := shaWeight(MatrixConfig{}); got != defaultSHAWeight {
		t.Errorf("unset weight should default to %d, got %d", defaultSHAWeight, got)
	}

	zero, fifty := 0, 50
	if got := shaWeight(MatrixConfig{SHAImageWeight: &zero}); got != 0 {
		t.Errorf("explicit 0 should stay 0, got %d", got)
	}
	if got := shaWeight(MatrixConfig{SHAImageWeight: &fifty}); got != 50 {
		t.Errorf("explicit 50 should stay 50, got %d", got)
	}
}

func TestShouldUseSHAEdges(t *testing.T) {
	date := time.Date(2026, 5, 8, 0, 0, 0, 0, time.UTC)

	for day := 1; day <= 60; day++ {
		d := date.AddDate(0, 0, day)
		if shouldUseSHA(0, d, shaOffsetServer) {
			t.Fatalf("weight 0 should never select SHA (day %d)", day)
		}
		if !shouldUseSHA(100, d, shaOffsetServer) {
			t.Fatalf("weight 100 should always select SHA (day %d)", day)
		}
	}
}

func TestShouldUseSHADistribution(t *testing.T) {
	sha := 0
	for day := 1; day <= 365; day++ {
		d := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).AddDate(0, 0, day-1)
		if shouldUseSHA(defaultSHAWeight, d, shaOffsetServer) {
			sha++
		}
	}

	// Roughly the configured 25%. Wide bounds, this is a seeded roll, not a
	// statistical guarantee.
	pct := 100 * sha / 365
	if pct < 15 || pct > 35 {
		t.Errorf("expected roughly %d%% SHA runs, got %d%% (%d/365)", defaultSHAWeight, pct, sha)
	}
	t.Logf("SHA selected on %d of 365 days (%d%%)", sha, pct)
}

func TestShouldUseSHAIsDeterministic(t *testing.T) {
	date := time.Date(2026, 6, 12, 0, 0, 0, 0, time.UTC)
	expected := shouldUseSHA(defaultSHAWeight, date, shaOffsetServer)
	for i := 0; i < 10; i++ {
		if got := shouldUseSHA(defaultSHAWeight, date, shaOffsetServer); got != expected {
			t.Fatal("same date must produce the same choice")
		}
	}
}

// The ticket requires sha->sha, tag->sha and sha->tag upgrades to all occur.
func TestSHACombinationsAllOccur(t *testing.T) {
	seen := make(map[string]int)
	for day := 1; day <= 365; day++ {
		d := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).AddDate(0, 0, day-1)
		from := shouldUseSHA(defaultSHAWeight, d, shaOffsetServer)
		to := shouldUseSHA(defaultSHAWeight, d, shaOffsetUpgrade)
		seen[shaLabel(from)+"->"+shaLabel(to)]++
	}

	for _, combo := range []string{"tag->tag", "tag->sha", "sha->tag", "sha->sha"} {
		if seen[combo] == 0 {
			t.Errorf("combination %s never occurred over 365 days: %v", combo, seen)
		}
	}
	t.Logf("upgrade combinations over 365 days: %v", seen)
}

func shaLabel(isSHA bool) string {
	if isSHA {
		return "sha"
	}
	return "tag"
}

func TestToDigestRef(t *testing.T) {
	const dg = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

	tests := []struct {
		name  string
		image string
		want  string
	}{
		{"tagged ghcr image", "ghcr.io/cb-vanilla/server:8.5.0", "ghcr.io/cb-vanilla/server@" + dg},
		{"tagged redhat image", "registry.connect.redhat.com/couchbase/server:7.6.12", "registry.connect.redhat.com/couchbase/server@" + dg},
		{"no tag", "ghcr.io/cb-vanilla/server", "ghcr.io/cb-vanilla/server@" + dg},
		{"registry with port, no tag", "localhost:5000/server", "localhost:5000/server@" + dg},
		{"registry with port and tag", "localhost:5000/server:8.5.0", "localhost:5000/server@" + dg},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := toDigestRef(tt.image, dg); got != tt.want {
				t.Errorf("toDigestRef(%q) = %q, want %q", tt.image, got, tt.want)
			}
		})
	}
}

// Offline mode must not reach the registry, so images stay tagged.
func TestOfflineModeKeepsTags(t *testing.T) {
	config := loadTestConfig(t)
	date := time.Date(2026, 6, 12, 0, 0, 0, 0, time.UTC)

	out := generateMatrix(config, date, true, "")
	for name, img := range map[string]string{
		"server_image":         out.ServerImage,
		"server_image_upgrade": out.ServerImageUpgrade,
	} {
		if strings.Contains(img, "@sha256:") {
			t.Errorf("%s should stay tagged in offline mode, got %s", name, img)
		}
	}
}

// The plain versions must always be present, whether the image is a tag or a
// digest. Without them the test framework cannot tell what version a digest is.
func TestServerImageVersionsAlwaysSet(t *testing.T) {
	config := loadTestConfig(t)

	for day := 1; day <= 60; day++ {
		date := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).AddDate(0, 0, day-1)
		out := generateMatrix(config, date, true, "")

		if out.ServerImageVersion == "" {
			t.Fatalf("%s: server_image_version is empty", date.Format("2006-01-02"))
		}
		if out.ServerImageUpgradeVersion == "" {
			t.Fatalf("%s: server_image_upgrade_version is empty", date.Format("2006-01-02"))
		}

		// Offline mode emits tags, so the tag must match the reported version.
		if want := ":" + out.ServerImageVersion; !strings.HasSuffix(out.ServerImage, want) {
			t.Errorf("%s: server_image %q does not end in %q", date.Format("2006-01-02"), out.ServerImage, want)
		}
		if want := ":" + out.ServerImageUpgradeVersion; !strings.HasSuffix(out.ServerImageUpgrade, want) {
			t.Errorf("%s: server_image_upgrade %q does not end in %q", date.Format("2006-01-02"), out.ServerImageUpgrade, want)
		}
	}
}

// The version fields describe the images, so they must survive a digest swap.
func TestVersionsUnaffectedByDigestSwap(t *testing.T) {
	config := loadTestConfig(t)
	date := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)

	out := generateMatrix(config, date, true, "")
	if out.ServerImageVersion != "8.0.2" {
		t.Errorf("expected server_image_version 8.0.2 for this date, got %s", out.ServerImageVersion)
	}
	if out.ServerImageUpgradeVersion != "7.2.9" {
		t.Errorf("expected server_image_upgrade_version 7.2.9 for this date, got %s", out.ServerImageUpgradeVersion)
	}
}

func TestValidSHAWeight(t *testing.T) {
	for _, w := range []int{0, 1, 25, 99, 100} {
		if !validSHAWeight(w) {
			t.Errorf("%d should be a valid weight", w)
		}
	}
	for _, w := range []int{-100, -2, -1, 101, 250} {
		if validSHAWeight(w) {
			t.Errorf("%d should be rejected", w)
		}
	}
}

// Only GHCR can be authenticated, so other registries keep their tag and the
// registry is never contacted. This test would hang or fail on a lookup.
func TestMaybeSHAImageSkipsNonGHCR(t *testing.T) {
	date := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	redhat := "registry.connect.redhat.com/couchbase/server:8.0.2"

	// weight 100 forces the roll to pick SHA, so only the registry check can
	// be what keeps the tag.
	got := maybeSHAImage(redhat, 100, date, shaOffsetServer, "", "", "server_image")
	if got != redhat {
		t.Errorf("non-GHCR image should keep its tag, got %s", got)
	}
}

func TestMaybeSHAImageKeepsTagWhenRollSaysTag(t *testing.T) {
	date := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	img := "ghcr.io/cb-vanilla/server:8.0.2"

	// weight 0 never selects SHA, so no lookup happens.
	if got := maybeSHAImage(img, 0, date, shaOffsetServer, "", "", "server_image"); got != img {
		t.Errorf("expected tag unchanged, got %s", got)
	}
}
