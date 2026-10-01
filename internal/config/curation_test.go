package config

import "testing"

func TestCurationCapabilityAndBudget(t *testing.T) {
	c := coldStartConfig()
	c.Workers[0].TaskTypes = append(c.Workers[0].TaskTypes, "curate")
	c.Tasks.Curate = Task{Timeout: 240}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if c.Task("curate").Timeout != 240 || c.Task("explore").ConcludeTimeout != 60 {
		t.Fatal("curator did not receive its own execution budget")
	}
	c.Tasks.Curate.Timeout = -1
	if err := c.Validate(); err == nil {
		t.Fatal("negative curation timeout accepted")
	}
}

func TestCurationRemainsOptionalForLegacyConfigurations(t *testing.T) {
	c := coldStartConfig()
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	c.Workers[0].TaskTypes = append(c.Workers[0].TaskTypes, "curate", "curate")
	if err := c.Validate(); err == nil {
		t.Fatal("duplicate curation capability accepted")
	}
}
