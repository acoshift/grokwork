package ghpr

import (
	"strings"
	"testing"
)

func TestParseWorkflowDispatchScalar(t *testing.T) {
	spec, err := ParseWorkflowDispatch([]byte(`
name: CI
on: workflow_dispatch
jobs:
  build:
    runs-on: ubuntu-latest
    steps: []
`))
	if err != nil {
		t.Fatal(err)
	}
	if !spec.Dispatchable {
		t.Fatal("expected dispatchable")
	}
	if len(spec.Inputs) != 0 {
		t.Fatalf("inputs=%+v", spec.Inputs)
	}
}

func TestParseWorkflowDispatchSequence(t *testing.T) {
	spec, err := ParseWorkflowDispatch([]byte(`
on: [push, workflow_dispatch]
`))
	if err != nil {
		t.Fatal(err)
	}
	if !spec.Dispatchable {
		t.Fatal("expected dispatchable")
	}
}

func TestParseWorkflowDispatchSequenceNoDispatch(t *testing.T) {
	spec, err := ParseWorkflowDispatch([]byte(`
on: [push, pull_request]
`))
	if err != nil {
		t.Fatal(err)
	}
	if spec.Dispatchable {
		t.Fatal("expected not dispatchable")
	}
}

func TestParseWorkflowDispatchMappingInputs(t *testing.T) {
	spec, err := ParseWorkflowDispatch([]byte(`
name: Deploy
on:
  workflow_dispatch:
    inputs:
      environment:
        description: Target environment
        required: true
        type: choice
        options:
          - staging
          - production
        default: staging
      dry_run:
        description: Skip apply
        type: boolean
        default: false
      note:
        type: string
      replicas:
        type: number
        default: 3
`))
	if err != nil {
		t.Fatal(err)
	}
	if !spec.Dispatchable {
		t.Fatal("expected dispatchable")
	}
	if len(spec.Inputs) != 4 {
		t.Fatalf("inputs=%+v", spec.Inputs)
	}
	byName := map[string]DispatchInput{}
	for _, in := range spec.Inputs {
		byName[in.Name] = in
	}
	env := byName["environment"]
	if env.Description != "Target environment" || !env.Required || env.Type != "choice" {
		t.Fatalf("environment=%+v", env)
	}
	if env.Default != "staging" || len(env.Options) != 2 || env.Options[0] != "staging" {
		t.Fatalf("environment options/default=%+v", env)
	}
	dry := byName["dry_run"]
	if dry.Type != "boolean" || dry.Default != "false" {
		t.Fatalf("dry_run=%+v", dry)
	}
	if byName["note"].Type != "string" {
		t.Fatalf("note=%+v", byName["note"])
	}
	if byName["replicas"].Type != "number" || byName["replicas"].Default != "3" {
		t.Fatalf("replicas=%+v", byName["replicas"])
	}
}

func TestParseWorkflowDispatchOnTrueQuirk(t *testing.T) {
	// Unquoted bare `on` is parsed by yaml.v3 as boolean key true.
	spec, err := ParseWorkflowDispatch([]byte("name: X\non: workflow_dispatch\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !spec.Dispatchable {
		t.Fatal("expected dispatchable with on→true quirk")
	}
}

func TestParseWorkflowDispatchOnTrueQuirkMapping(t *testing.T) {
	// Force the bool-key form by using a YAML that yaml.v3 decodes with true key.
	// Writing `on:` unquoted is enough; also cover quoted "on".
	spec, err := ParseWorkflowDispatch([]byte(`
"on":
  workflow_dispatch:
    inputs:
      name:
        default: world
`))
	if err != nil {
		t.Fatal(err)
	}
	if !spec.Dispatchable || len(spec.Inputs) != 1 || spec.Inputs[0].Name != "name" {
		t.Fatalf("%+v", spec)
	}
}

func TestParseWorkflowDispatchNoOn(t *testing.T) {
	spec, err := ParseWorkflowDispatch([]byte(`
name: orphan
jobs: {}
`))
	if err != nil {
		t.Fatal(err)
	}
	if spec.Dispatchable {
		t.Fatal("expected not dispatchable")
	}
}

func TestParseWorkflowDispatchPushOnly(t *testing.T) {
	spec, err := ParseWorkflowDispatch([]byte(`
on: push
`))
	if err != nil {
		t.Fatal(err)
	}
	if spec.Dispatchable {
		t.Fatal("expected not dispatchable")
	}
}

func TestParseWorkflowDispatchBadYAML(t *testing.T) {
	_, err := ParseWorkflowDispatch([]byte(":\n  -"))
	if err == nil {
		t.Fatal("expected parse error")
	}
}

func TestParseWorkflowDispatchMappingWithoutInputs(t *testing.T) {
	spec, err := ParseWorkflowDispatch([]byte(`
on:
  workflow_dispatch:
  push:
`))
	if err != nil {
		t.Fatal(err)
	}
	if !spec.Dispatchable {
		t.Fatal("expected dispatchable")
	}
	if len(spec.Inputs) != 0 {
		t.Fatalf("inputs=%+v", spec.Inputs)
	}
}

func TestParseWorkflowDispatchDegradesUnknownPieces(t *testing.T) {
	spec, err := ParseWorkflowDispatch([]byte(`
on:
  workflow_dispatch:
    inputs:
      weird:
        type: mystery
        required: maybe
        options: not-a-list
        default: [a, b]
`))
	if err != nil {
		t.Fatal(err)
	}
	if !spec.Dispatchable || len(spec.Inputs) != 1 {
		t.Fatalf("%+v", spec)
	}
	in := spec.Inputs[0]
	if in.Name != "weird" {
		t.Fatalf("%+v", in)
	}
	// Unknown type falls back to string; non-scalar default ignored; options may be empty or single.
	if in.Type != "string" {
		t.Fatalf("type=%q", in.Type)
	}
	if in.Required {
		t.Fatal("required should be false for non-bool")
	}
	if in.Default != "" {
		t.Fatalf("default should be empty for sequence: %q", in.Default)
	}
}

func TestParseWorkflowDispatchPreservesInputOrder(t *testing.T) {
	spec, err := ParseWorkflowDispatch([]byte(`
on:
  workflow_dispatch:
    inputs:
      z: {}
      a: {}
      m: {}
`))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, in := range spec.Inputs {
		names = append(names, in.Name)
	}
	if strings.Join(names, ",") != "z,a,m" {
		t.Fatalf("order=%v", names)
	}
}
