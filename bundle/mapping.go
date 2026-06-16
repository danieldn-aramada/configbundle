package bundle

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// MappingPayload is the JSON-encoded body of a MediaTypeMapping OCI layer.
//
// Both cb-bundler (producer) and cb-controller (consumer) import this type —
// neither side hand-rolls the wire shape. A version bump on the media type
// implies a new exported type (e.g. MappingPayloadV2).
type MappingPayload struct {
	BundleDigest string        `json:"bundleDigest"`
	Rules        []MappingRule `json:"rules"`
}

// MappingRule declares how to translate a K8s field path under a nested
// configurable unit into orbital identity (orbId + type). One rule per nested
// type — N parent instances all reuse the same rule.
//
// Example: a rule with ListField="spec.servers", ItemKey="orbId", Field="idrac",
// Type="IdracSettings", OrbIDSuffix="-idrac" matches the path
//
//	spec.servers[orbId=colo:GQK3V64].idrac.sshEnabled
//
// and yields orbId="colo:GQK3V64-idrac", type="IdracSettings", field="sshEnabled".
type MappingRule struct {
	// ListField is the dotted path to the parent list in the CR spec, e.g.
	// "spec.servers". The list must declare ItemKey as its listMapKey.
	ListField string `json:"listField"`

	// ItemKey is the field name used as the listMapKey on items in ListField.
	// Today's CRDs use "orbId" exclusively.
	ItemKey string `json:"itemKey"`

	// Field is the JSON name of the nested struct on each list item, e.g.
	// "idrac". One rule covers one nested field.
	Field string `json:"field"`

	// Type is the orbital type name for divergence attribution, e.g.
	// "IdracSettings". Sent verbatim in the OverrideEntry.Type field.
	Type string `json:"type"`

	// OrbIDSuffix is appended to the parent item's ItemKey value to derive
	// the nested entity's orbId. Today the only suffix in use is "-idrac".
	// Convention: nested orbital entities are named "<parent-orbId><suffix>".
	OrbIDSuffix string `json:"orbIdSuffix"`
}

// ParseMappingPayload deserializes and validates the JSON payload of a
// MediaTypeMapping OCI layer. Returns an error if the JSON is malformed, if
// there are no rules, or if any rule has a missing required field.
func ParseMappingPayload(b []byte) (*MappingPayload, error) {
	var p MappingPayload
	if err := json.Unmarshal(b, &p); err != nil {
		return nil, fmt.Errorf("decode mapping: %w", err)
	}
	if len(p.Rules) == 0 {
		return nil, errors.New("mapping has no rules")
	}
	for i, r := range p.Rules {
		if r.ListField == "" || r.ItemKey == "" || r.Field == "" || r.Type == "" || r.OrbIDSuffix == "" {
			return nil, fmt.Errorf("rule %d: all of listField, itemKey, field, type, orbIdSuffix are required", i)
		}
	}
	return &p, nil
}

// Resolve walks the rules and matches the K8s field path against each. On
// match it returns the nested entity's orbId, the leaf field name (the
// configurable scalar), and the orbital type name. Returns an error when no
// rule matches.
//
// The path format produced by the divergence reporter is:
//
//	<listField>[<itemKey>=<itemKeyValue>].<field>.<leaf>
//
// e.g. "spec.servers[orbId=colo:GQK3V64].idrac.sshEnabled".
func (p *MappingPayload) Resolve(path string) (orbID, field, typeName string, err error) {
	if p == nil {
		return "", "", "", errors.New("nil mapping")
	}
	for _, rule := range p.Rules {
		itemKeyValue, leaf, ok := matchRule(path, rule)
		if !ok {
			continue
		}
		return itemKeyValue + rule.OrbIDSuffix, leaf, rule.Type, nil
	}
	return "", "", "", fmt.Errorf("no mapping rule matches path %q", path)
}

// ResolveByOrbID finds the rule that owns a given nested orbId by stripping
// the rule's OrbIDSuffix to recover the parent's ItemKey value. Returns the
// matching rule and the parent itemKey value, so callers (e.g. buildTakeover,
// applyOmissions) can navigate to the parent list item without re-parsing.
func (p *MappingPayload) ResolveByOrbID(orbID string) (rule MappingRule, parentItemKey string, ok bool) {
	if p == nil {
		return MappingRule{}, "", false
	}
	for _, r := range p.Rules {
		if r.OrbIDSuffix == "" {
			continue
		}
		if parent, found := strings.CutSuffix(orbID, r.OrbIDSuffix); found && parent != "" {
			return r, parent, true
		}
	}
	return MappingRule{}, "", false
}

// matchRule tries to parse path as <rule.ListField>[<rule.ItemKey>=X].<rule.Field>.<leaf>
// and returns (X, leaf, true) on success. The leaf must be a single dotted
// segment with no further nesting — divergences are reported at field-leaf
// granularity, not at deeper paths.
func matchRule(path string, rule MappingRule) (itemKeyValue, leaf string, ok bool) {
	prefix := rule.ListField + "[" + rule.ItemKey + "="
	rest, found := strings.CutPrefix(path, prefix)
	if !found {
		return "", "", false
	}
	closeIdx := strings.Index(rest, "]")
	if closeIdx < 0 {
		return "", "", false
	}
	itemKeyValue = rest[:closeIdx]
	if itemKeyValue == "" {
		return "", "", false
	}
	afterClose := rest[closeIdx+1:]
	fieldPrefix := "." + rule.Field + "."
	rest2, found := strings.CutPrefix(afterClose, fieldPrefix)
	if !found {
		return "", "", false
	}
	// Leaf must be a single segment — no further dots or brackets.
	if strings.ContainsAny(rest2, ".[") {
		return "", "", false
	}
	return itemKeyValue, rest2, true
}
