package ghpr

import (
	"context"
	"fmt"
	"path"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// DispatchSpec describes whether a workflow YAML supports workflow_dispatch
// and the inputs it declares.
//
// Parsed from the workflow file as it exists on the project primary branch.
// GitHub actually dispatches the definition on the chosen ref; primary is an
// acceptable approximation for the form UI (shared checkouts are routinely dirty).
type DispatchSpec struct {
	Dispatchable bool
	Inputs       []DispatchInput
}

// DispatchInput is one workflow_dispatch input.
type DispatchInput struct {
	Name        string
	Description string
	Required    bool
	Default     string
	// Type is string, boolean, choice, environment, or number (default string).
	Type    string
	Options []string // choice only
}

// ParseWorkflowDispatch inspects a GitHub Actions workflow YAML for a
// workflow_dispatch trigger and its inputs. A file with no such trigger
// returns {Dispatchable: false} and a nil error. YAML parse failures return err.
//
// on: may be a scalar, sequence, or mapping. Bare `on` is also accepted as a
// boolean key true (yaml.v3 quirk).
func ParseWorkflowDispatch(yamlSrc []byte) (DispatchSpec, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(yamlSrc, &doc); err != nil {
		return DispatchSpec{}, err
	}
	root := &doc
	if doc.Kind == yaml.DocumentNode {
		if len(doc.Content) == 0 {
			return DispatchSpec{}, nil
		}
		root = doc.Content[0]
	}
	if root == nil || root.Kind != yaml.MappingNode {
		return DispatchSpec{}, nil
	}
	onNode := findWorkflowOnNode(root)
	if onNode == nil {
		return DispatchSpec{}, nil
	}
	dispatchNode, ok := findWorkflowDispatch(onNode)
	if !ok {
		return DispatchSpec{}, nil
	}
	spec := DispatchSpec{Dispatchable: true}
	if dispatchNode != nil {
		spec.Inputs = parseDispatchInputs(dispatchNode)
	}
	return spec, nil
}

// WorkflowFileAtPrimary reads a workflow file from the committed tree of the
// origin primary (never the working tree).
func WorkflowFileAtPrimary(ctx context.Context, repoDir, workflowPath string) ([]byte, error) {
	return WorkflowFileAtPrimaryWith(ctx, defaultRunner, repoDir, workflowPath)
}

// WorkflowFileAtPrimaryWith is WorkflowFileAtPrimary with an injectable runner.
// Resolves refs/remotes/origin/HEAD, then origin/main, then origin/master via
// git rev-parse --verify, and reads with git cat-file blob <ref>:<path>.
func WorkflowFileAtPrimaryWith(ctx context.Context, run Runner, repoDir, workflowPath string) ([]byte, error) {
	if run == nil {
		run = defaultRunner
	}
	workflowPath = strings.TrimSpace(workflowPath)
	if workflowPath == "" {
		return nil, fmt.Errorf("empty workflow path")
	}
	// Keep the path as a single tree path. Reject absolute paths and ".." before
	// Clean, which would otherwise collapse "../x" into "x" and hide the intent.
	normalized := strings.ReplaceAll(workflowPath, "\\", "/")
	if path.IsAbs(normalized) || strings.Contains(normalized, "..") {
		return nil, fmt.Errorf("invalid workflow path %q", workflowPath)
	}
	clean := path.Clean(normalized)
	if clean == "" || clean == "." || strings.HasPrefix(clean, "../") {
		return nil, fmt.Errorf("invalid workflow path %q", workflowPath)
	}
	workflowPath = clean

	ref, err := resolveOriginPrimaryRef(ctx, run, repoDir)
	if err != nil {
		return nil, err
	}
	raw, err := run(ctx, repoDir, "git", "cat-file", "blob", ref+":"+workflowPath)
	if err != nil {
		return nil, fmt.Errorf("read workflow %s at %s: %w", workflowPath, shortRef(ref), err)
	}
	return raw, nil
}

func resolveOriginPrimaryRef(ctx context.Context, run Runner, repoDir string) (string, error) {
	for _, cand := range []string{
		"refs/remotes/origin/HEAD",
		"origin/main",
		"origin/master",
	} {
		out, err := run(ctx, repoDir, "git", "rev-parse", "--verify", cand)
		if err != nil {
			continue
		}
		ref := strings.TrimSpace(string(out))
		if ref != "" {
			return ref, nil
		}
	}
	return "", fmt.Errorf("no origin primary ref (tried origin/HEAD, origin/main, origin/master)")
}

func shortRef(ref string) string {
	if len(ref) > 12 {
		return ref[:12]
	}
	return ref
}

// findWorkflowOnNode returns the value node for the top-level `on` key.
// yaml.v3 parses bare `on` as a boolean key true — accept "on", "true", or bool true.
func findWorkflowOnNode(root *yaml.Node) *yaml.Node {
	if root == nil || root.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(root.Content); i += 2 {
		k, v := root.Content[i], root.Content[i+1]
		if isYAMLOnKey(k) {
			return v
		}
	}
	return nil
}

func isYAMLOnKey(n *yaml.Node) bool {
	if n == nil {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(n.Value)) {
	case "on", "true":
		return true
	}
	var b bool
	if err := n.Decode(&b); err == nil && b {
		return true
	}
	return false
}

// findWorkflowDispatch reports whether on: includes workflow_dispatch.
// When the trigger is a mapping entry, the value node (inputs parent) is returned.
func findWorkflowDispatch(on *yaml.Node) (*yaml.Node, bool) {
	if on == nil {
		return nil, false
	}
	switch on.Kind {
	case yaml.ScalarNode:
		return nil, strings.TrimSpace(on.Value) == "workflow_dispatch"
	case yaml.SequenceNode:
		for _, item := range on.Content {
			if item != nil && item.Kind == yaml.ScalarNode && strings.TrimSpace(item.Value) == "workflow_dispatch" {
				return nil, true
			}
		}
		return nil, false
	case yaml.MappingNode:
		for i := 0; i+1 < len(on.Content); i += 2 {
			k, v := on.Content[i], on.Content[i+1]
			if k != nil && strings.TrimSpace(k.Value) == "workflow_dispatch" {
				return v, true
			}
		}
		return nil, false
	default:
		return nil, false
	}
}

func parseDispatchInputs(dispatch *yaml.Node) []DispatchInput {
	if dispatch == nil || dispatch.Kind != yaml.MappingNode {
		return nil
	}
	var inputsNode *yaml.Node
	for i := 0; i+1 < len(dispatch.Content); i += 2 {
		k, v := dispatch.Content[i], dispatch.Content[i+1]
		if k != nil && strings.TrimSpace(k.Value) == "inputs" {
			inputsNode = v
			break
		}
	}
	if inputsNode == nil || inputsNode.Kind != yaml.MappingNode {
		return nil
	}
	out := make([]DispatchInput, 0, len(inputsNode.Content)/2)
	for i := 0; i+1 < len(inputsNode.Content); i += 2 {
		k, v := inputsNode.Content[i], inputsNode.Content[i+1]
		if k == nil {
			continue
		}
		name := strings.TrimSpace(k.Value)
		if name == "" {
			continue
		}
		inp := DispatchInput{Name: name, Type: "string"}
		if v != nil && v.Kind == yaml.MappingNode {
			fillDispatchInput(&inp, v)
		}
		out = append(out, inp)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func fillDispatchInput(inp *DispatchInput, node *yaml.Node) {
	for i := 0; i+1 < len(node.Content); i += 2 {
		k, v := node.Content[i], node.Content[i+1]
		if k == nil || v == nil {
			continue
		}
		switch strings.TrimSpace(k.Value) {
		case "description":
			inp.Description = scalarString(v)
		case "required":
			inp.Required = scalarBool(v)
		case "default":
			inp.Default = scalarString(v)
		case "type":
			if t := normalizeInputType(scalarString(v)); t != "" {
				inp.Type = t
			}
		case "options":
			inp.Options = scalarStringList(v)
		}
	}
}

func normalizeInputType(t string) string {
	switch strings.ToLower(strings.TrimSpace(t)) {
	case "string", "boolean", "choice", "environment", "number":
		return strings.ToLower(strings.TrimSpace(t))
	default:
		return ""
	}
}

func scalarString(n *yaml.Node) string {
	if n == nil {
		return ""
	}
	switch n.Kind {
	case yaml.ScalarNode:
		// Prefer the decoded Go value so bools/numbers stringify consistently.
		var v any
		if err := n.Decode(&v); err == nil {
			switch x := v.(type) {
			case string:
				return x
			case bool:
				return strconv.FormatBool(x)
			case int:
				return strconv.Itoa(x)
			case int64:
				return strconv.FormatInt(x, 10)
			case uint64:
				return strconv.FormatUint(x, 10)
			case float64:
				return strconv.FormatFloat(x, 'f', -1, 64)
			}
		}
		return n.Value
	default:
		return ""
	}
}

func scalarBool(n *yaml.Node) bool {
	if n == nil {
		return false
	}
	var b bool
	if err := n.Decode(&b); err == nil {
		return b
	}
	s := strings.ToLower(strings.TrimSpace(n.Value))
	return s == "true" || s == "yes" || s == "1"
}

func scalarStringList(n *yaml.Node) []string {
	if n == nil {
		return nil
	}
	if n.Kind != yaml.SequenceNode {
		if s := scalarString(n); s != "" {
			return []string{s}
		}
		return nil
	}
	out := make([]string, 0, len(n.Content))
	for _, item := range n.Content {
		if s := scalarString(item); s != "" {
			out = append(out, s)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
