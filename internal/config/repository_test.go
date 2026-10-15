package config

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
	"unicode"
	"unicode/utf8"
)

func TestRepositoryREADMEBrandImagesExist(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("..", "..", "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	picture := regexp.MustCompile(`(?s)<picture>.*?</picture>`).Find(body)
	images := regexp.MustCompile(`\b(?:src|srcset)="(\./[^"]+)"`).FindAllSubmatch(picture, -1)
	if len(images) != 2 {
		t.Fatalf("README brand picture must reference light and dark images, got %d", len(images))
	}
	for _, image := range images {
		path := string(image[1])
		t.Run(path, func(t *testing.T) {
			info, err := os.Stat(filepath.Join("..", "..", filepath.FromSlash(path)))
			if err != nil {
				t.Fatal(err)
			}
			if !info.Mode().IsRegular() || info.Size() == 0 {
				t.Fatal("README brand image must be a nonempty file")
			}
		})
	}
}

func TestRepositoryDocumentationContainsNoChinese(t *testing.T) {
	for _, file := range []string{
		"README.md", "SECURITY.md", "LICENSE", "container/README.md", "container/environment.md",
		"Dockerfile", "container/Dockerfile", "compose.yaml", "dispatch.example.yaml", ".env.example",
		".gitignore", ".gitattributes", ".dockerignore", "tests/integration/README.md",
		"docs/architecture.md", "docs/development.md", "docs/formats.md", "docs/protocol.md", "docs/recovery.md",
	} {
		t.Run(file, func(t *testing.T) {
			body, err := os.ReadFile(filepath.Join("..", "..", filepath.FromSlash(file)))
			if err != nil {
				t.Fatal(err)
			}
			if !utf8.Valid(body) {
				t.Fatal("documentation must be valid UTF-8")
			}
			line := 1
			for _, r := range string(body) {
				if unicode.Is(unicode.Han, r) {
					t.Fatalf("untranslated Chinese character %q on line %d", r, line)
				}
				if r == '\n' {
					line++
				}
			}
		})
	}
}

// Exercise the shipped example through the same loader as the Dispatcher, using
// synthetic environment values so these checks never need local credentials.
func TestRepositoryDispatchConfigurations(t *testing.T) {
	env := map[string]string{
		"ANTHROPIC_AUTH_TOKEN":          "configuration-test-token",
		"ANTHROPIC_BASE_URL":            "https://model.example.invalid",
		"ANTHROPIC_DEFAULT_FABLE_MODEL": "configuration-test-model",
	}
	for key, value := range env {
		t.Setenv(key, value)
	}
	for _, file := range []string{"dispatch.example.yaml"} {
		t.Run(file, func(t *testing.T) {
			c, err := Load(filepath.Join("..", "..", file))
			if err != nil {
				t.Fatal(err)
			}
			for key := range env {
				if c.CommonEnv[key] != "${"+key+"}" {
					t.Errorf("%s must remain an environment reference", key)
				}
			}
			for _, w := range c.Workers {
				for key, want := range env {
					if w.Env[key] != want {
						t.Errorf("worker %s did not inherit %s from the environment", w.Name, key)
					}
				}
				if w.Env["ANTHROPIC_MODEL"] != env["ANTHROPIC_DEFAULT_FABLE_MODEL"] {
					t.Errorf("worker %s lost the configured model alias", w.Name)
				}
				if w.Env["PWNMESH_MAX_OUTPUT_TOKENS"] != "384000" || w.Env["PWNMESH_REASONING_EFFORT"] != "max" {
					t.Errorf("worker %s lost the configured model output budget", w.Name)
				}
			}
			if c.Task("reason").Timeout != 300 || c.Task("explore").Timeout != 0 || c.Task("explore").ConcludeTimeout != 60 {
				t.Fatal("configured task deadlines changed")
			}
		})
	}
}
