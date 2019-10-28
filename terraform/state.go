package terraform

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"sync"

	"github.com/hashicorp/hcl2/hcl"
	"github.com/hashicorp/hcl2/hcl/hclsyntax"
	"github.com/hashicorp/terraform-plugin-sdk/internal/addrs"
	"github.com/hashicorp/terraform-plugin-sdk/internal/configs/hcl2shim"
	"github.com/hashicorp/terraform-plugin-sdk/internal/tfdiags"
	"github.com/mitchellh/copystructure"
	"github.com/zclconf/go-cty/cty"
)

const (
	// StateVersion is the current version for our state file
	StateVersion = 3
)

// rootModulePath is the path of the root module
var rootModulePath = []string{"root"}

// normalizeModulePath transforms a legacy module path (which may or may not
// have a redundant "root" label at the start of it) into an
// addrs.ModuleInstance representing the same module.
//
// For legacy reasons, different parts of Terraform disagree about whether the
// root module has the path []string{} or []string{"root"}, and so this
// function accepts both and trims off the "root". An implication of this is
// that it's not possible to actually have a module call in the root module
// that is itself named "root", since that would be ambiguous.
//
// normalizeModulePath takes a raw module path and returns a path that
// has the rootModulePath prepended to it. If I could go back in time I
// would've never had a rootModulePath (empty path would be root). We can
// still fix this but thats a big refactor that my branch doesn't make sense
// for. Instead, this function normalizes paths.
func normalizeModulePath(p []string) addrs.ModuleInstance {
	// FIXME: Remove this once everyone is using addrs.ModuleInstance.

	if len(p) > 0 && p[0] == "root" {
		p = p[1:]
	}

	ret := make(addrs.ModuleInstance, len(p))
	for i, name := range p {
		// For now we don't actually support modules with multiple instances
		// identified by keys, so we just treat every path element as a
		// step with no key.
		ret[i] = addrs.ModuleInstanceStep{
			Name: name,
		}
	}
	return ret
}

// ResourceState holds the state of a resource that is used so that
// a provider can find and manage an existing resource as well as for
// storing attributes that are used to populate variables of child
// resources.
//
// Attributes has attributes about the created resource that are
// queryable in interpolation: "${type.id.attr}"
//
// Extra is just extra data that a provider can return that we store
// for later, but is not exposed in any way to the user.
//
type ResourceState struct {
	// This is filled in and managed by Terraform, and is the resource
	// type itself such as "mycloud_instance". If a resource provider sets
	// this value, it won't be persisted.
	Type string `json:"type"`

	// Dependencies are a list of things that this resource relies on
	// existing to remain intact. For example: an AWS instance might
	// depend on a subnet (which itself might depend on a VPC, and so
	// on).
	//
	// Terraform uses this information to build valid destruction
	// orders and to warn the user if they're destroying a resource that
	// another resource depends on.
	//
	// Things can be put into this list that may not be managed by
	// Terraform. If Terraform doesn't find a matching ID in the
	// overall state, then it assumes it isn't managed and doesn't
	// worry about it.
	Dependencies []string `json:"depends_on"`

	// Primary is the current active instance for this resource.
	// It can be replaced but only after a successful creation.
	// This is the instances on which providers will act.
	Primary *InstanceState `json:"primary"`

	// Deposed is used in the mechanics of CreateBeforeDestroy: the existing
	// Primary is Deposed to get it out of the way for the replacement Primary to
	// be created by Apply. If the replacement Primary creates successfully, the
	// Deposed instance is cleaned up.
	//
	// If there were problems creating the replacement Primary, the Deposed
	// instance and the (now tainted) replacement Primary will be swapped so the
	// tainted replacement will be cleaned up instead.
	//
	// An instance will remain in the Deposed list until it is successfully
	// destroyed and purged.
	Deposed []*InstanceState `json:"deposed"`

	// Provider is used when a resource is connected to a provider with an alias.
	// If this string is empty, the resource is connected to the default provider,
	// e.g. "aws_instance" goes with the "aws" provider.
	// If the resource block contained a "provider" key, that value will be set here.
	Provider string `json:"provider"`

	mu sync.Mutex
}

func (s *ResourceState) Lock()   { s.mu.Lock() }
func (s *ResourceState) Unlock() { s.mu.Unlock() }

// Equal tests whether two ResourceStates are equal.
func (s *ResourceState) Equal(other *ResourceState) bool {
	s.Lock()
	defer s.Unlock()

	if s.Type != other.Type {
		return false
	}

	if s.Provider != other.Provider {
		return false
	}

	// Dependencies must be equal
	sort.Strings(s.Dependencies)
	sort.Strings(other.Dependencies)
	if len(s.Dependencies) != len(other.Dependencies) {
		return false
	}
	for i, d := range s.Dependencies {
		if other.Dependencies[i] != d {
			return false
		}
	}

	// States must be equal
	if !s.Primary.Equal(other.Primary) {
		return false
	}

	return true
}

// Taint marks a resource as tainted.
func (s *ResourceState) Taint() {
	s.Lock()
	defer s.Unlock()

	if s.Primary != nil {
		s.Primary.Tainted = true
	}
}

// Untaint unmarks a resource as tainted.
func (s *ResourceState) Untaint() {
	s.Lock()
	defer s.Unlock()

	if s.Primary != nil {
		s.Primary.Tainted = false
	}
}

// ProviderAddr returns the provider address for the receiver, by parsing the
// string representation saved in state. An error can be returned if the
// value in state is corrupt.
func (s *ResourceState) ProviderAddr() (addrs.AbsProviderConfig, error) {
	var diags tfdiags.Diagnostics

	str := s.Provider
	traversal, travDiags := hclsyntax.ParseTraversalAbs([]byte(str), "", hcl.Pos{Line: 1, Column: 1})
	diags = diags.Append(travDiags)
	if travDiags.HasErrors() {
		return addrs.AbsProviderConfig{}, diags.Err()
	}

	addr, addrDiags := addrs.ParseAbsProviderConfig(traversal)
	diags = diags.Append(addrDiags)
	return addr, diags.Err()
}

func (s *ResourceState) String() string {
	s.Lock()
	defer s.Unlock()

	var buf bytes.Buffer
	buf.WriteString(fmt.Sprintf("Type = %s", s.Type))
	return buf.String()
}

// InstanceState is used to track the unique state information belonging
// to a given instance.
type InstanceState struct {
	// A unique ID for this resource. This is opaque to Terraform
	// and is only meant as a lookup mechanism for the providers.
	ID string `json:"id"`

	// Attributes are basic information about the resource. Any keys here
	// are accessible in variable format within Terraform configurations:
	// ${resourcetype.name.attribute}.
	Attributes map[string]string `json:"attributes"`

	// Ephemeral is used to store any state associated with this instance
	// that is necessary for the Terraform run to complete, but is not
	// persisted to a state file.
	Ephemeral EphemeralState `json:"-"`

	// Meta is a simple K/V map that is persisted to the State but otherwise
	// ignored by Terraform core. It's meant to be used for accounting by
	// external client code. The value here must only contain Go primitives
	// and collections.
	Meta map[string]interface{} `json:"meta"`

	// Tainted is used to mark a resource for recreation.
	Tainted bool `json:"tainted"`

	mu sync.Mutex
}

func (s *InstanceState) Lock()   { s.mu.Lock() }
func (s *InstanceState) Unlock() { s.mu.Unlock() }

func (s *InstanceState) init() {
	s.Lock()
	defer s.Unlock()

	if s.Attributes == nil {
		s.Attributes = make(map[string]string)
	}
	if s.Meta == nil {
		s.Meta = make(map[string]interface{})
	}
	s.Ephemeral.init()
}

// NewInstanceStateShimmedFromValue is a shim method to lower a new-style
// object value representing the attributes of an instance object into the
// legacy InstanceState representation.
//
// This is for shimming to old components only and should not be used in new code.
func NewInstanceStateShimmedFromValue(state cty.Value, schemaVersion int) *InstanceState {
	attrs := hcl2shim.FlatmapValueFromHCL2(state)
	return &InstanceState{
		ID:         attrs["id"],
		Attributes: attrs,
		Meta: map[string]interface{}{
			"schema_version": schemaVersion,
		},
	}
}

// AttrsAsObjectValue shims from the legacy InstanceState representation to
// a new-style cty object value representation of the state attributes, using
// the given type for guidance.
//
// The given type must be the implied type of the schema of the resource type
// of the object whose state is being converted, or the result is undefined.
//
// This is for shimming from old components only and should not be used in
// new code.
func (s *InstanceState) AttrsAsObjectValue(ty cty.Type) (cty.Value, error) {
	if s == nil {
		// if the state is nil, we need to construct a complete cty.Value with
		// null attributes, rather than a single cty.NullVal(ty)
		s = &InstanceState{}
	}

	if s.Attributes == nil {
		s.Attributes = map[string]string{}
	}

	// make sure ID is included in the attributes. The InstanceState.ID value
	// takes precedence.
	if s.ID != "" {
		s.Attributes["id"] = s.ID
	}

	return hcl2shim.HCL2ValueFromFlatmap(s.Attributes, ty)
}

// Copy all the Fields from another InstanceState
func (s *InstanceState) Set(from *InstanceState) {
	s.Lock()
	defer s.Unlock()

	from.Lock()
	defer from.Unlock()

	s.ID = from.ID
	s.Attributes = from.Attributes
	s.Ephemeral = from.Ephemeral
	s.Meta = from.Meta
	s.Tainted = from.Tainted
}

func (s *InstanceState) DeepCopy() *InstanceState {
	copy, err := copystructure.Config{Lock: true}.Copy(s)
	if err != nil {
		panic(err)
	}

	return copy.(*InstanceState)
}

func (s *InstanceState) Empty() bool {
	if s == nil {
		return true
	}
	s.Lock()
	defer s.Unlock()

	return s.ID == ""
}

func (s *InstanceState) Equal(other *InstanceState) bool {
	// Short circuit some nil checks
	if s == nil || other == nil {
		return s == other
	}
	s.Lock()
	defer s.Unlock()

	// IDs must be equal
	if s.ID != other.ID {
		return false
	}

	// Attributes must be equal
	if len(s.Attributes) != len(other.Attributes) {
		return false
	}
	for k, v := range s.Attributes {
		otherV, ok := other.Attributes[k]
		if !ok {
			return false
		}

		if v != otherV {
			return false
		}
	}

	// Meta must be equal
	if len(s.Meta) != len(other.Meta) {
		return false
	}
	if s.Meta != nil && other.Meta != nil {
		// We only do the deep check if both are non-nil. If one is nil
		// we treat it as equal since their lengths are both zero (check
		// above).
		//
		// Since this can contain numeric values that may change types during
		// serialization, let's compare the serialized values.
		sMeta, err := json.Marshal(s.Meta)
		if err != nil {
			// marshaling primitives shouldn't ever error out
			panic(err)
		}
		otherMeta, err := json.Marshal(other.Meta)
		if err != nil {
			panic(err)
		}

		if !bytes.Equal(sMeta, otherMeta) {
			return false
		}
	}

	if s.Tainted != other.Tainted {
		return false
	}

	return true
}

// MergeDiff takes a ResourceDiff and merges the attributes into
// this resource state in order to generate a new state. This new
// state can be used to provide updated attribute lookups for
// variable interpolation.
//
// If the diff attribute requires computing the value, and hence
// won't be available until apply, the value is replaced with the
// computeID.
func (s *InstanceState) MergeDiff(d *InstanceDiff) *InstanceState {
	result := s.DeepCopy()
	if result == nil {
		result = new(InstanceState)
	}
	result.init()

	if s != nil {
		s.Lock()
		defer s.Unlock()
		for k, v := range s.Attributes {
			result.Attributes[k] = v
		}
	}
	if d != nil {
		for k, diff := range d.CopyAttributes() {
			if diff.NewRemoved {
				delete(result.Attributes, k)
				continue
			}
			if diff.NewComputed {
				result.Attributes[k] = hcl2shim.UnknownVariableValue
				continue
			}

			result.Attributes[k] = diff.New
		}
	}

	return result
}

func (s *InstanceState) String() string {
	notCreated := "<not created>"

	if s == nil {
		return notCreated
	}

	s.Lock()
	defer s.Unlock()

	var buf bytes.Buffer

	if s.ID == "" {
		return notCreated
	}

	buf.WriteString(fmt.Sprintf("ID = %s\n", s.ID))

	attributes := s.Attributes
	attrKeys := make([]string, 0, len(attributes))
	for ak, _ := range attributes {
		if ak == "id" {
			continue
		}

		attrKeys = append(attrKeys, ak)
	}
	sort.Strings(attrKeys)

	for _, ak := range attrKeys {
		av := attributes[ak]
		buf.WriteString(fmt.Sprintf("%s = %s\n", ak, av))
	}

	buf.WriteString(fmt.Sprintf("Tainted = %t\n", s.Tainted))

	return buf.String()
}

// EphemeralState is used for transient state that is only kept in-memory
type EphemeralState struct {
	// ConnInfo is used for the providers to export information which is
	// used to connect to the resource for provisioning. For example,
	// this could contain SSH or WinRM credentials.
	ConnInfo map[string]string `json:"-"`

	// Type is used to specify the resource type for this instance. This is only
	// required for import operations (as documented). If the documentation
	// doesn't state that you need to set this, then don't worry about
	// setting it.
	Type string `json:"-"`
}

func (e *EphemeralState) init() {
	if e.ConnInfo == nil {
		e.ConnInfo = make(map[string]string)
	}
}

func (e *EphemeralState) DeepCopy() *EphemeralState {
	copy, err := copystructure.Config{Lock: true}.Copy(e)
	if err != nil {
		panic(err)
	}

	return copy.(*EphemeralState)
}
