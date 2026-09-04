package config

import "testing"

func TestParseConfigBytesNormalizesFactory(t *testing.T) {
	cfg, err := ParseConfigBytes([]byte(`
factory:
  enabled: true
  python-command: "  /opt/factory/bin/python  "
  droid-command: "  /opt/factory/bin/droid  "
  cwd: "  /workspace  "
  prefix: "  team-factory/  "
  excluded-models: [" Disabled-Model ", "disabled-model", "other"]
  autonomy: " HIGH "
  tools: [" Read ", "Read", "Execute", ""]
  enable-builtin-skills: true
`))
	if err != nil {
		t.Fatalf("ParseConfigBytes() error = %v", err)
	}
	factory := cfg.Factory
	if !factory.Enabled {
		t.Fatal("Factory.Enabled = false, want true")
	}
	if factory.PythonCommand != "/opt/factory/bin/python" {
		t.Fatalf("Factory.PythonCommand = %q", factory.PythonCommand)
	}
	if factory.DroidCommand != "/opt/factory/bin/droid" {
		t.Fatalf("Factory.DroidCommand = %q", factory.DroidCommand)
	}
	if factory.CWD != "/workspace" {
		t.Fatalf("Factory.CWD = %q", factory.CWD)
	}
	if factory.Prefix != "team-factory" {
		t.Fatalf("Factory.Prefix = %q", factory.Prefix)
	}
	if len(factory.ExcludedModels) != 2 || factory.ExcludedModels[0] != "disabled-model" || factory.ExcludedModels[1] != "other" {
		t.Fatalf("Factory.ExcludedModels = %#v", factory.ExcludedModels)
	}
	if factory.Autonomy != "high" || len(factory.Tools) != 2 || factory.Tools[0] != "Read" || factory.Tools[1] != "Execute" || !factory.EnableBuiltinSkills {
		t.Fatalf("Factory agent controls = %#v", factory)
	}
}

func TestParseConfigBytesDefaultsEnabledFactoryCommands(t *testing.T) {
	cfg, err := ParseConfigBytes([]byte("factory:\n  enabled: true\n"))
	if err != nil {
		t.Fatalf("ParseConfigBytes() error = %v", err)
	}
	if cfg.Factory.PythonCommand != "python3" {
		t.Fatalf("Factory.PythonCommand = %q, want python3", cfg.Factory.PythonCommand)
	}
	if cfg.Factory.DroidCommand != "droid" {
		t.Fatalf("Factory.DroidCommand = %q, want droid", cfg.Factory.DroidCommand)
	}
	if cfg.Factory.Prefix != "factory" {
		t.Fatalf("Factory.Prefix = %q, want factory", cfg.Factory.Prefix)
	}
	if cfg.Factory.Autonomy != "off" || len(cfg.Factory.Tools) != 0 || cfg.Factory.EnableBuiltinSkills {
		t.Fatalf("Factory safe defaults = %#v", cfg.Factory)
	}
}

func TestParseConfigBytesRejectsInvalidFactoryAutonomy(t *testing.T) {
	_, err := ParseConfigBytes([]byte("factory:\n  enabled: true\n  autonomy: root\n"))
	if err == nil {
		t.Fatal("ParseConfigBytes() error = nil")
	}
}

func TestFactoryExampleConfigParses(t *testing.T) {
	cfg, err := LoadConfig("../../config.example.yaml")
	if err != nil {
		t.Fatalf("LoadConfig(config.example.yaml) error = %v", err)
	}
	if cfg.Factory.Enabled {
		t.Fatal("example Factory backend must be disabled by default")
	}
	if cfg.Factory.Autonomy != "off" || len(cfg.Factory.Tools) != 0 {
		t.Fatalf("example Factory safety defaults = %#v", cfg.Factory)
	}
}
