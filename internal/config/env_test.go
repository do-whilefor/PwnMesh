package config

import (
	"os"
	"testing"
)

func TestGetenvLegacyFallback(t *testing.T) {
	for _, tc := range []struct {
		name, key, legacyKey, value, legacyValue, want string
		setValue, setLegacy                            bool
	}{
		{name: "missing", key: "PWNMESH_TEST_SETTING", legacyKey: "XLOOM_TEST_SETTING"},
		{name: "legacy", key: "PWNMESH_TEST_SETTING", legacyKey: "XLOOM_TEST_SETTING", setLegacy: true, legacyValue: "old", want: "old"},
		{name: "canonical", key: "PWNMESH_TEST_SETTING", legacyKey: "XLOOM_TEST_SETTING", setValue: true, value: "new", want: "new"},
		{name: "canonical wins", key: "PWNMESH_TEST_SETTING", legacyKey: "XLOOM_TEST_SETTING", setValue: true, value: "new", setLegacy: true, legacyValue: "old", want: "new"},
		{name: "empty canonical wins", key: "PWNMESH_TEST_SETTING", legacyKey: "XLOOM_TEST_SETTING", setValue: true, setLegacy: true, legacyValue: "old"},
		{name: "unrelated value", key: "CONFIG_TEST_SETTING", legacyKey: "XLOOM_CONFIG_TEST_SETTING", setValue: true, value: "direct", setLegacy: true, legacyValue: "old", want: "direct"},
		{name: "unrelated missing", key: "CONFIG_TEST_SETTING", legacyKey: "XLOOM_CONFIG_TEST_SETTING", setLegacy: true, legacyValue: "old"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, key := range []string{tc.key, tc.legacyKey} {
				t.Setenv(key, "") // Restore the original process environment on cleanup.
				if err := os.Unsetenv(key); err != nil {
					t.Fatal(err)
				}
			}
			if tc.setValue {
				t.Setenv(tc.key, tc.value)
			}
			if tc.setLegacy {
				t.Setenv(tc.legacyKey, tc.legacyValue)
			}
			if got := Getenv(tc.key); got != tc.want {
				t.Errorf("Getenv returned %q, want %q", got, tc.want)
			}
		})
	}
}

func TestConfigNormalizesLegacyWorkerEnvironment(t *testing.T) {
	const canonical = "PWNMESH_CONTEXT_BYTES"
	const legacy = "XLOOM_CONTEXT_BYTES"
	for _, tc := range []struct {
		name           string
		common, worker map[string]string
		want           string
	}{
		{name: "common legacy", common: map[string]string{legacy: "100"}, want: "100"},
		{name: "worker legacy overrides same common key", common: map[string]string{legacy: "100"}, worker: map[string]string{legacy: "200"}, want: "200"},
		{name: "worker canonical overrides same common key", common: map[string]string{canonical: "100"}, worker: map[string]string{canonical: "200"}, want: "200"},
		{name: "canonical common wins over legacy worker", common: map[string]string{canonical: "100"}, worker: map[string]string{legacy: "200"}, want: "100"},
		{name: "canonical worker wins over legacy common", common: map[string]string{legacy: "100"}, worker: map[string]string{canonical: "200"}, want: "200"},
		{name: "empty canonical common wins", common: map[string]string{canonical: ""}, worker: map[string]string{legacy: "200"}},
		{name: "empty canonical worker wins", common: map[string]string{legacy: "100"}, worker: map[string]string{canonical: ""}},
		{name: "expanded legacy value", common: map[string]string{legacy: "${PWNMESH_CONFIG_TEST_VALUE}"}, want: "300"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("PWNMESH_CONFIG_TEST_VALUE", "300")
			c := coldStartConfig()
			for key, value := range tc.common {
				c.CommonEnv[key] = value
			}
			c.Workers[0].Env = tc.worker
			if err := c.Validate(); err != nil {
				t.Fatal(err)
			}
			if got, exists := c.Workers[0].Env[canonical]; !exists || got != tc.want {
				t.Fatalf("canonical setting = %q, exists = %v; want %q", got, exists, tc.want)
			}
			if _, initiallyCanonical := tc.common[canonical]; !initiallyCanonical {
				if _, addedToCommon := c.CommonEnv[canonical]; addedToCommon {
					t.Fatal("normalization changed common configuration")
				}
			}
		})
	}
}
