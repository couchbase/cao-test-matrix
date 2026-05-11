package main

import (
	"encoding/json"
	"os"
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
		{Version: "8.0.0", Weight: 5},
		{Version: "7.6.0", Weight: 4},
		{Version: "7.2.0", Weight: 2},
		{Version: "7.1.0", Weight: 1},
		{Version: "7.0.0", Weight: 1},
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

	// 8.0.0 should be selected more than 7.0.0
	if counts["8.0.0"] <= counts["7.0.0"] {
		t.Errorf("8.0.0 (weight=5) should be selected more than 7.0.0 (weight=1): got %d vs %d",
			counts["8.0.0"], counts["7.0.0"])
	}

	t.Logf("Distribution over 365 days: %v", counts)
}

func TestUpgradePathSelection(t *testing.T) {
	upgradePaths := map[string][]string{
		"8.0.0": {"7.6.0", "7.2.0"},
		"7.6.0": {"7.2.0", "7.1.0"},
		"7.2.0": {"7.1.0", "7.0.0"},
		"7.1.0": {"7.0.0"},
		"7.0.0": {},
	}

	date := time.Date(2026, 5, 8, 0, 0, 0, 0, time.UTC)

	// 8.0.0 should upgrade from 7.6.0 or 7.2.0
	upgrade := selectUpgradeVersion(upgradePaths, "8.0.0", date)
	if upgrade != "7.6.0" && upgrade != "7.2.0" {
		t.Errorf("8.0.0 upgrade should be 7.6.0 or 7.2.0, got %s", upgrade)
	}

	// 7.0.0 has no upgrade path, should return itself
	upgrade = selectUpgradeVersion(upgradePaths, "7.0.0", date)
	if upgrade != "7.0.0" {
		t.Errorf("7.0.0 should return itself (no upgrade path), got %s", upgrade)
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
		"8.0.0": {"7.6.0", "7.2.0"},
		"7.6.0": {"7.2.0", "7.1.0"},
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

	k8sImage := resolveServerImage("8.0.0", k8sPlatform)
	if k8sImage != "ghcr.io/cb-vanilla/server:8.0.0" {
		t.Errorf("Expected ghcr.io/cb-vanilla/server:8.0.0, got %s", k8sImage)
	}

	ocImage := resolveServerImage("7.6.0", ocPlatform)
	if ocImage != "registry.connect.redhat.com/couchbase/server:7.6.0" {
		t.Errorf("Expected registry.connect.redhat.com/couchbase/server:7.6.0, got %s", ocImage)
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

			// All sidecars except operator should use :latest
			for name, img := range map[string]string{
				"admission": admission, "cert": cert, "backup": backup,
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
