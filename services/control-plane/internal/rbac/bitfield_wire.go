package rbac

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
)

// errBitfieldSign refuses a permission bitfield string that starts with '+'.
// #3406 (D2) and docs/api/openapi.yaml promise a 400 for that spelling. Go 1.26
// refused it inside encoding/json. Go 1.27 runs encoding/json on the v2 engine,
// which parses a ",string" integer with strconv.ParseInt and so reads "+64" as
// 64, and no GODEBUG setting or go directive restores the old answer.
var errBitfieldSign = errors.New("permission bitfield must be decimal digits without a sign")

// bitfieldSignProbe stands in for a bitfield member in a shadow decode.
// encoding/json calls its UnmarshalJSON once for EVERY occurrence of the member,
// after its own member matching (case folding, escapes), so a signed occurrence
// is refused even when a later duplicate would be the value stored. Checking
// only the last occurrence let "+64" followed by null store 64.
type bitfieldSignProbe struct {
	last json.RawMessage // the final occurrence, as written
}

// UnmarshalJSON refuses a JSON string whose decoded value starts with '+'. A
// value that is not a string passes: the typed decode refuses it, or a null
// leaves the field unchanged there, as it did on Go 1.26.
func (p *bitfieldSignProbe) UnmarshalJSON(b []byte) error {
	b = bytes.TrimLeft(b, " \t\r\n")
	p.last = append(p.last[:0], b...)
	if len(b) == 0 || b[0] != '"' {
		return nil
	}
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	if strings.HasPrefix(s, "+") {
		return errBitfieldSign
	}
	return nil
}

// quotedNull reports whether the final occurrence was a JSON string whose value
// is "null", however it was escaped. UnmarshalJSON already decoded that string
// without error, so a decode failure here cannot happen and reads as false.
func (p *bitfieldSignProbe) quotedNull() bool {
	var s string
	return len(p.last) > 0 && p.last[0] == '"' && json.Unmarshal(p.last, &s) == nil && s == "null"
}

// permissionsShadow repeats the role requests' bitfield member as a probe.
type permissionsShadow struct {
	Permissions bitfieldSignProbe `json:"permissions"`
}

// UnmarshalJSON refuses a signed permissions string, then decodes as usual.
func (r *CreateRoleRequest) UnmarshalJSON(data []byte) error {
	if err := json.Unmarshal(data, &permissionsShadow{}); err != nil {
		return err
	}
	type plain CreateRoleRequest
	return json.Unmarshal(data, (*plain)(r))
}

// UnmarshalJSON refuses a signed permissions string, then decodes as usual.
func (r *UpdateRoleRequest) UnmarshalJSON(data []byte) error {
	var shadow permissionsShadow
	if err := json.Unmarshal(data, &shadow); err != nil {
		return err
	}
	type plain UpdateRoleRequest
	if err := json.Unmarshal(data, (*plain)(r)); err != nil {
		return err
	}
	// Go 1.27 clears a ",string" pointer only for the literal token "null". An
	// escaped spelling of the same string leaves a present 0, or an earlier
	// value, where Go 1.26 cleared it, and a present Permissions makes UpdateRole
	// write the role's permissions. nil leaves them unchanged.
	if shadow.Permissions.quotedNull() {
		r.Permissions = nil
	}
	return nil
}

// UnmarshalJSON refuses a signed allow or deny string, then decodes as usual.
func (r *UpsertOverrideRequest) UnmarshalJSON(data []byte) error {
	var shadow struct {
		Allow bitfieldSignProbe `json:"allow"`
		Deny  bitfieldSignProbe `json:"deny"`
	}
	if err := json.Unmarshal(data, &shadow); err != nil {
		return err
	}
	type plain UpsertOverrideRequest
	return json.Unmarshal(data, (*plain)(r))
}
