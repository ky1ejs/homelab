package main

import (
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Obsidian Bases: YAML files with the .base extension that define filtered,
// sorted views over the vault's frontmatter. They are the one non-markdown file
// this server reads and writes, and the reason is that they are authored text in
// the same sense a note is — a query the user writes, not configuration Obsidian
// keeps for itself. See obsidian-vault/DECISIONS.md#bases-are-documents.
//
// Two things make a base different from a note, and both are handled here:
//
//  1. A base has nowhere to put the agent stamp. YAML frontmatter at the top of
//     a .base is a second document that breaks the file, and a stamp written as
//     root keys would be dropped the next time Obsidian re-serializes it from
//     the UI. So a base is written unstamped, like an attachment move — the
//     hole is recorded in docs/shared-contract.md.
//  2. A malformed base fails silently in the one place anyone looks: Obsidian
//     shows an error banner instead of the view, on the phone, later. So every
//     write is parsed first and refused if it would not load.
//
// The schema checked below is the one in the vault's own reference note (the
// BASES_DOC setting), which is the same note the model is told to read.

const baseExt = ".base"

// ErrNotNote is append or capture aimed at a base. Appending text to the end of
// a YAML file is not a meaningful edit of it: the result is a stray scalar under
// whatever key happens to be last, or a parse error.
var ErrNotNote = errors.New("append is for notes, not bases")

// BaseError is a write refused because the result would not load in Obsidian.
// The message is written to be handed back to the model as-is.
type BaseError struct{ Problems []string }

func (e *BaseError) Error() string {
	return "base would not load: " + strings.Join(e.Problems, "; ")
}

func isBase(path string) bool {
	return strings.EqualFold(filepath.Ext(path), baseExt)
}

func isNote(path string) bool {
	return strings.EqualFold(filepath.Ext(path), ".md")
}

// isDocument is "a file this server reads and writes as text": notes and bases.
// Everything else is either an attachment, which only Move touches, or refused.
func isDocument(path string) bool {
	return isNote(path) || isBase(path)
}

// Root keys the Bases syntax defines. Anything else is reported, not refused:
// a plugin or a newer app version may write keys the reference does not list,
// and refusing an edit to a base Obsidian itself wrote would be worse than a
// warning about a key it will ignore.
var baseRootKeys = map[string]bool{
	"filters": true, "formulas": true, "properties": true, "summaries": true, "views": true,
}

// The summary names the app provides without a root `summaries` entry.
var builtinSummaries = map[string]bool{
	"Average": true, "Min": true, "Max": true, "Sum": true, "Range": true,
	"Median": true, "Stddev": true, "Earliest": true, "Latest": true,
	"Checked": true, "Unchecked": true, "Empty": true, "Filled": true, "Unique": true,
}

// A formula reference inside a property reference or an expression. Bracket
// syntax (formula["x y"]) is not matched; formula names are identifiers in
// practice, and a missed reference only costs a warning.
var formulaRef = regexp.MustCompile(`\bformula\.([A-Za-z_][A-Za-z0-9_]*)`)

// ValidateBase parses a base and checks it against the Bases schema.
//
// Problems that stop the base loading are returned as a *BaseError. Problems
// that load but render wrongly — a formula column with no formula behind it is
// the common one, and it renders as an empty column with no banner — come back
// as warnings for the caller to pass on. The split is deliberate: only what the
// reference says breaks a base is refused, so this can never be stricter than
// Obsidian about a file Obsidian wrote.
func ValidateBase(content string) (warnings []string, err error) {
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(content), &doc); err != nil {
		return nil, &BaseError{Problems: []string{"the YAML does not parse: " + yamlMessage(err)}}
	}
	if doc.Kind == 0 || len(doc.Content) == 0 {
		return nil, &BaseError{Problems: []string{"the file is empty; a base needs at least a 'views' list"}}
	}
	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		return nil, &BaseError{Problems: []string{"the top level must be a mapping of keys such as 'filters' and 'views'"}}
	}
	// yaml.v3 accepts a repeated key when decoding into a Node; the YAML parsers
	// Obsidian ships do not. Checked here because it is the shape a careless
	// edit produces — a second 'views:' appended rather than merged — and the
	// one the parse above cannot see.
	if dup := duplicateKey(root); dup != "" {
		return nil, &BaseError{Problems: []string{"the YAML does not parse: " + dup}}
	}

	var problems []string
	formulas := map[string]bool{}
	summaries := map[string]bool{}
	refs := map[string]string{} // formula name -> where it was used, first sighting

	noteRefs := func(where, s string) {
		for _, m := range formulaRef.FindAllStringSubmatch(s, -1) {
			if _, seen := refs[m[1]]; !seen {
				refs[m[1]] = where
			}
		}
	}

	// First pass for the definitions, so a reference can be checked whatever
	// order the keys appear in.
	if n := mapGet(root, "formulas"); n != nil {
		if n.Kind != yaml.MappingNode {
			problems = append(problems, "'formulas' must be a mapping of name to expression")
		} else {
			for i := 0; i+1 < len(n.Content); i += 2 {
				name, expr := n.Content[i].Value, n.Content[i+1]
				formulas[name] = true
				if expr.Kind != yaml.ScalarNode {
					problems = append(problems, fmt.Sprintf("formula %q must be a single expression string", name))
				}
			}
		}
	}
	if n := mapGet(root, "summaries"); n != nil {
		if n.Kind != yaml.MappingNode {
			problems = append(problems, "'summaries' must be a mapping of name to expression")
		} else {
			for i := 0; i+1 < len(n.Content); i += 2 {
				summaries[n.Content[i].Value] = true
			}
		}
	}

	for i := 0; i+1 < len(root.Content); i += 2 {
		key, val := root.Content[i].Value, root.Content[i+1]
		switch key {
		case "filters":
			problems = append(problems, checkFilter("filters", val)...)
		case "formulas":
			// Formulas may reference each other, and an expression naming a
			// formula that does not exist renders empty like any other.
			for j := 0; j+1 < len(val.Content); j += 2 {
				noteRefs("formula "+val.Content[j].Value, val.Content[j+1].Value)
			}
		case "properties":
			if val.Kind != yaml.MappingNode {
				problems = append(problems, "'properties' must be a mapping keyed by property")
				continue
			}
			for j := 0; j+1 < len(val.Content); j += 2 {
				noteRefs("properties", val.Content[j].Value)
			}
		case "summaries", "views":
		default:
			warnings = append(warnings, fmt.Sprintf("top-level key %q is not part of the Bases syntax and will be ignored", key))
		}
	}

	views := mapGet(root, "views")
	switch {
	case views == nil:
		problems = append(problems, "there is no 'views' list; a base needs at least one view")
	case views.Kind != yaml.SequenceNode:
		problems = append(problems, "'views' must be a list")
	case len(views.Content) == 0:
		problems = append(problems, "'views' is empty; a base needs at least one view")
	default:
		for i, v := range views.Content {
			p, w := checkView(i, v, summaries, noteRefs)
			problems = append(problems, p...)
			warnings = append(warnings, w...)
		}
	}

	if len(problems) > 0 {
		return nil, &BaseError{Problems: problems}
	}

	// Sorted so the reply is stable: map order would make two identical writes
	// produce differently worded results.
	missing := make([]string, 0, len(refs))
	for name := range refs {
		if !formulas[name] {
			missing = append(missing, name)
		}
	}
	sort.Strings(missing)
	for _, name := range missing {
		warnings = append(warnings, fmt.Sprintf("formula.%s is used in %s but not defined under 'formulas', so it will render empty", name, refs[name]))
	}
	return warnings, nil
}

func checkView(i int, v *yaml.Node, summaries map[string]bool, noteRefs func(where, s string)) (problems, warnings []string) {
	label := fmt.Sprintf("view %d", i+1)
	if v.Kind != yaml.MappingNode {
		return []string{label + " must be a mapping with at least a 'type'"}, nil
	}
	if n := mapGet(v, "name"); n != nil && n.Value != "" {
		label = fmt.Sprintf("view %q", n.Value)
	}
	if t := mapGet(v, "type"); t == nil || t.Kind != yaml.ScalarNode || strings.TrimSpace(t.Value) == "" {
		problems = append(problems, label+" has no 'type' (table, cards, list, map or kanban)")
	}

	// Views carry app-written state keys this does not know about — rowHeight,
	// columnSize, image, cardSize and others — so only the keys the syntax
	// defines are inspected, and everything else passes untouched.
	if n := mapGet(v, "filters"); n != nil {
		problems = append(problems, checkFilter(label+" filters", n)...)
	}
	if n := mapGet(v, "order"); n != nil {
		if n.Kind != yaml.SequenceNode {
			problems = append(problems, label+" 'order' must be a list of properties")
		} else {
			for _, p := range n.Content {
				noteRefs(label+" order", p.Value)
			}
		}
	}
	if n := mapGet(v, "sort"); n != nil {
		if n.Kind != yaml.SequenceNode {
			problems = append(problems, label+" 'sort' must be a list of {property, direction}")
		} else {
			for _, s := range n.Content {
				if s.Kind != yaml.MappingNode || mapGet(s, "property") == nil {
					problems = append(problems, label+" 'sort' entries need a 'property'")
					continue
				}
				noteRefs(label+" sort", mapGet(s, "property").Value)
				if d := mapGet(s, "direction"); d != nil && d.Value != "ASC" && d.Value != "DESC" {
					warnings = append(warnings, fmt.Sprintf("%s sort direction %q should be ASC or DESC", label, d.Value))
				}
			}
		}
	}
	if n := mapGet(v, "groupBy"); n != nil {
		if n.Kind != yaml.MappingNode || mapGet(n, "property") == nil {
			problems = append(problems, label+" 'groupBy' must be a mapping with one 'property'")
		} else {
			noteRefs(label+" groupBy", mapGet(n, "property").Value)
		}
	}
	if n := mapGet(v, "summaries"); n != nil {
		if n.Kind != yaml.MappingNode {
			problems = append(problems, label+" 'summaries' must map a property to a summary name")
		} else {
			for j := 0; j+1 < len(n.Content); j += 2 {
				prop, name := n.Content[j].Value, n.Content[j+1].Value
				noteRefs(label+" summaries", prop)
				if !builtinSummaries[name] && !summaries[name] {
					warnings = append(warnings, fmt.Sprintf("%s summarises %s with %q, which is neither a built-in summary nor defined under the top-level 'summaries'", label, prop, name))
				}
			}
		}
	}
	return problems, warnings
}

// checkFilter enforces the one filter grammar the reference defines: a single
// expression string, or a mapping with exactly one of and/or/not holding a list
// of expressions and nested filters. Expressions themselves are not parsed —
// that is Obsidian's expression language, and a half-implemented copy of it
// would refuse valid bases.
func checkFilter(where string, n *yaml.Node) []string {
	switch n.Kind {
	case yaml.ScalarNode:
		if strings.TrimSpace(n.Value) == "" {
			return []string{where + " is an empty expression"}
		}
		return nil
	case yaml.MappingNode:
		if len(n.Content) != 2 {
			return []string{where + " must have exactly one of 'and', 'or' or 'not'"}
		}
		op, list := n.Content[0].Value, n.Content[1]
		if op != "and" && op != "or" && op != "not" {
			return []string{fmt.Sprintf("%s uses %q; a filter group is 'and', 'or' or 'not'", where, op)}
		}
		if list.Kind != yaml.SequenceNode {
			return []string{fmt.Sprintf("%s '%s' must hold a list", where, op)}
		}
		var problems []string
		for _, item := range list.Content {
			problems = append(problems, checkFilter(where+" "+op, item)...)
		}
		return problems
	default:
		return []string{where + " must be an expression string or an and/or/not group"}
	}
}

// duplicateKey describes the first key repeated within one mapping anywhere in
// the tree, or returns "".
func duplicateKey(n *yaml.Node) string {
	if n.Kind == yaml.MappingNode {
		seen := map[string]bool{}
		for i := 0; i+1 < len(n.Content); i += 2 {
			k := n.Content[i].Value
			if seen[k] {
				return fmt.Sprintf("line %d: key %q appears twice in the same mapping", n.Content[i].Line, k)
			}
			seen[k] = true
		}
	}
	for _, c := range n.Content {
		if d := duplicateKey(c); d != "" {
			return d
		}
	}
	return ""
}

// mapGet returns the value under key in a mapping node, or nil.
func mapGet(m *yaml.Node, key string) *yaml.Node {
	if m == nil || m.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

// yamlMessage drops the library's "yaml: " prefix, which reads as noise when
// spoken and adds nothing a model needs.
func yamlMessage(err error) string {
	return strings.TrimPrefix(err.Error(), "yaml: ")
}
