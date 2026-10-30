// Package voiceauthprotocol implements the closed, pure protocol-4 wire boundary.
// Decoding and proof verification confer no SQL or native authority.
package voiceauthprotocol

// Endpoint identifies one of the seven fixed HTTP protocol routes.
type Endpoint string

// Supported endpoints derive their fixed POST paths inside the codec.
const (
	EndpointConnectionRegister Endpoint = "connection_register"
	EndpointConnectionRelease  Endpoint = "connection_release"
	EndpointProvisionalClose   Endpoint = "provisional_close"
	EndpointRoomRegister       Endpoint = "room_register"
	EndpointRoomRelease        Endpoint = "room_release"
	EndpointHealthBootstrap    Endpoint = "health_bootstrap"
	EndpointBootRetire         Endpoint = "boot_retire"
)

// Header preserves one header tuple so duplicate security headers remain detectable.
type Header struct {
	Name  string
	Value string
}

// HTTPRequest retains the exact method, request URI, header tuples and body bytes.
type HTTPRequest struct {
	Method  string
	Path    string
	Headers []Header
	Body    []byte
}

// HTTPResponse contains the status, header tuples and exact response body bytes.
type HTTPResponse struct {
	Status  int
	Headers []Header
	Body    []byte
}

// AttemptMeta contains caller-selected canonical timestamp and nonce values.
type AttemptMeta struct {
	Timestamp string
	Nonce     string
}

// ExpectedResponse supplies independently known identity for response binding checks.
type ExpectedResponse struct {
	UserID  string
	Binding *Binding
}

// ProtocolError reports a fixed diagnostic class without payload or secret material.
type ProtocolError struct{ reason string }

func (e *ProtocolError) Error() string { return "voice auth protocol: " + e.reason }

// ProtocolReason returns the closed diagnostic classification, never a wire error code.
func (e *ProtocolError) ProtocolReason() string { return e.reason }
func failure(reason string) error               { return &ProtocolError{reason: reason} }

// Target identifies exactly one connection generation, boot, socket and epoch.
type Target struct {
	ConnectionGeneration string `json:"connectionGeneration"`
	NodeBootID           string `json:"nodeBootId"`
	SocketID             string `json:"socketId"`
	CredentialEpoch      string `json:"credentialEpoch"`
}

// Binding associates an exact connection target with its user and login family.
type Binding struct {
	UserID   string `json:"userId"`
	FamilyID string `json:"familyId"`
	Target   Target `json:"target"`
}

// Room identifies an immutable room session and its channel or DM resource.
type Room struct {
	SessionGeneration string `json:"sessionGeneration"`
	RoomID            string `json:"roomId"`
	RoomKind          string `json:"roomKind"`
}

// AuthCommand is the closed auth-revocation command payload.
type AuthCommand struct {
	UserID      string `json:"userId"`
	FamilyID    string `json:"familyId"`
	Target      Target `json:"target"`
	Schema      string `json:"schema"`
	Version     int    `json:"version"`
	OperationID string `json:"operationId"`
	Scope       string `json:"scope"`
	Reason      string `json:"reason"`
	Timestamp   string `json:"timestamp"`
	Nonce       string `json:"nonce"`
	Proof       string `json:"proof"`
}

// EpochCommand is the closed credential-connection-revocation command payload.
type EpochCommand struct {
	UserID                    string `json:"userId"`
	FamilyID                  string `json:"familyId"`
	Target                    Target `json:"target"`
	Schema                    string `json:"schema"`
	Version                   int    `json:"version"`
	ParentGeneration          string `json:"parentGeneration"`
	SupersededCredentialEpoch string `json:"supersededCredentialEpoch"`
	Timestamp                 string `json:"timestamp"`
	Nonce                     string `json:"nonce"`
	Proof                     string `json:"proof"`
}

// Health4 is the protocol-4 transport-health and admission-permission challenge.
type Health4 struct {
	Schema           string `json:"schema"`
	Version          int    `json:"version"`
	Protocol         int    `json:"protocol"`
	NodeBootID       string `json:"nodeBootId"`
	Challenge        string `json:"challenge"`
	Activation       string `json:"activation"`
	AdmissionAllowed bool   `json:"admissionAllowed"`
	Timestamp        string `json:"timestamp"`
	Proof            string `json:"proof"`
}

// ConnectionRegister carries a new connection target without caller-supplied user identity.
type ConnectionRegister struct {
	Version              int    `json:"version"`
	ConnectionGeneration string `json:"connectionGeneration"`
	NodeBootID           string `json:"nodeBootId"`
	SocketID             string `json:"socketId"`
	CredentialEpoch      string `json:"credentialEpoch"`
}

// RegistrationResult returns the control-plane-derived binding and protocol capability.
type RegistrationResult struct {
	UserID                   string `json:"userId"`
	FamilyID                 string `json:"familyId"`
	Target                   Target `json:"target"`
	Version                  int    `json:"version"`
	VoiceEnforcementProtocol int    `json:"voiceEnforcementProtocol"`
}

// ConnectionRelease requests closure of an exact known connection binding.
type ConnectionRelease struct {
	UserID   string `json:"userId"`
	FamilyID string `json:"familyId"`
	Target   Target `json:"target"`
	Version  int    `json:"version"`
}

// ProvisionalClose binds closure to a target and its original registration commitments.
type ProvisionalClose struct {
	Schema                  string `json:"schema"`
	Version                 int    `json:"version"`
	Target                  Target `json:"target"`
	RegistrationTokenDigest string `json:"registrationTokenDigest"`
	RegistrationBodyDigest  string `json:"registrationBodyDigest"`
}

// ProvisionalCloseResult reports a registered, unregistered or retired-boot closure result.
type ProvisionalCloseResult struct {
	Schema                  string   `json:"schema"`
	Version                 int      `json:"version"`
	Target                  Target   `json:"target"`
	RegistrationTokenDigest string   `json:"registrationTokenDigest"`
	RegistrationBodyDigest  string   `json:"registrationBodyDigest"`
	Disposition             string   `json:"disposition"`
	Binding                 *Binding `json:"binding,omitempty"`
}

// RoomRegister requests attachment or renewal of an exact connection and room tuple.
type RoomRegister struct {
	Schema               string `json:"schema"`
	Version              int    `json:"version"`
	Mode                 string `json:"mode"`
	ConnectionGeneration string `json:"connectionGeneration"`
	NodeBootID           string `json:"nodeBootId"`
	SocketID             string `json:"socketId"`
	CredentialEpoch      string `json:"credentialEpoch"`
	SessionGeneration    string `json:"sessionGeneration"`
	RoomID               string `json:"roomId"`
	RoomKind             string `json:"roomKind"`
}

// RoomRegistrationResult returns the registered connection binding and exact room tuple.
type RoomRegistrationResult struct {
	Schema                   string  `json:"schema"`
	Version                  int     `json:"version"`
	VoiceEnforcementProtocol int     `json:"voiceEnforcementProtocol"`
	Binding                  Binding `json:"binding"`
	Room                     Room    `json:"room"`
}

// RoomRelease requests closure of an exact connection and room association.
type RoomRelease struct {
	Schema  string  `json:"schema"`
	Version int     `json:"version"`
	Binding Binding `json:"binding"`
	Room    Room    `json:"room"`
}

// HealthBootstrap requests a protocol-4 capability check for one boot and nonce.
type HealthBootstrap struct {
	Version    int    `json:"version"`
	NodeBootID string `json:"nodeBootId"`
	Nonce      string `json:"nonce"`
}

// HealthBootstrapResult reports boot transport capability and productive admission state.
type HealthBootstrapResult struct {
	Version                  int    `json:"version"`
	VoiceEnforcementProtocol int    `json:"voiceEnforcementProtocol"`
	NodeBootID               string `json:"nodeBootId"`
	Activation               string `json:"activation"`
	TransportOK              bool   `json:"transportOK"`
	AdmissionGranted         bool   `json:"admissionGranted"`
}

// BootRetire requests irreversible retirement of an exact boot identity.
type BootRetire struct {
	Schema     string `json:"schema"`
	Version    int    `json:"version"`
	NodeBootID string `json:"nodeBootId"`
}

// AuthAck echoes an auth command with a separately authenticated successful acknowledgment.
type AuthAck struct {
	UserID      string `json:"userId"`
	FamilyID    string `json:"familyId"`
	Target      Target `json:"target"`
	Schema      string `json:"schema"`
	Version     int    `json:"version"`
	OperationID string `json:"operationId"`
	Scope       string `json:"scope"`
	Reason      string `json:"reason"`
	Timestamp   string `json:"timestamp"`
	Nonce       string `json:"nonce"`
	OK          bool   `json:"ok"`
	Proof       string `json:"proof"`
}

// EpochAck echoes an epoch command with a separately authenticated successful acknowledgment.
type EpochAck struct {
	UserID                    string `json:"userId"`
	FamilyID                  string `json:"familyId"`
	Target                    Target `json:"target"`
	Schema                    string `json:"schema"`
	Version                   int    `json:"version"`
	ParentGeneration          string `json:"parentGeneration"`
	SupersededCredentialEpoch string `json:"supersededCredentialEpoch"`
	Timestamp                 string `json:"timestamp"`
	Nonce                     string `json:"nonce"`
	OK                        bool   `json:"ok"`
	Proof                     string `json:"proof"`
}

// HealthAck4 echoes a health challenge with transport success and bounded admission permission.
type HealthAck4 struct {
	Schema           string `json:"schema"`
	Version          int    `json:"version"`
	Protocol         int    `json:"protocol"`
	NodeBootID       string `json:"nodeBootId"`
	Challenge        string `json:"challenge"`
	Activation       string `json:"activation"`
	AdmissionAllowed bool   `json:"admissionAllowed"`
	Timestamp        string `json:"timestamp"`
	TransportOK      bool   `json:"transportOK"`
	AdmissionGranted bool   `json:"admissionGranted"`
	Proof            string `json:"proof"`
}

// ResponsePayload seals the supported nonempty HTTP success payloads.
type ResponsePayload interface{ responsePayload() }

func (RegistrationResult) responsePayload() {
	// Marker method: seals ResponsePayload.
}

func (ProvisionalCloseResult) responsePayload() {
	// Marker method: seals ResponsePayload.
}

func (RoomRegistrationResult) responsePayload() {
	// Marker method: seals ResponsePayload.
}

func (HealthBootstrapResult) responsePayload() {
	// Marker method: seals ResponsePayload.
}

// Command retains a privately tagged command and its original raw bytes.
type Command struct {
	value any
	raw   []byte
}

// ACK retains a verified acknowledgment with a private variant tag.
type ACK struct{ value any }

// Request retains a decoded request and copied wire commitments; decoding is not verification.
type Request struct {
	endpoint                                         Endpoint
	value                                            any
	wire                                             HTTPRequest
	timestamp, nonce, proof, tokenDigest, bodyDigest string
}

// Response retains a verified success payload or an exact empty 204 result.
type Response struct {
	endpoint Endpoint
	value    any
	empty    bool
}

// RequestContext privately binds expected response identity to a retained original request.
type RequestContext struct {
	request  Request
	expected ExpectedResponse
	valid    bool
}

// Kind returns the exact schema discriminator, or empty for a zero wrapper.
func (w Command) Kind() string { return payloadSchema(w.value) }

// Kind returns the exact schema discriminator, or empty for a zero wrapper.
func (w ACK) Kind() string { return payloadSchema(w.value) }

// Endpoint returns the retained endpoint, or empty for a zero wrapper.
func (w Request) Endpoint() Endpoint { return w.endpoint }

// Endpoint returns the retained endpoint, or empty for a zero wrapper.
func (w Response) Endpoint() Endpoint { return w.endpoint }

// Empty reports whether this response is a successfully verified empty 204.
func (w Response) Empty() bool { return w.empty }

// Auth returns a payload copy and whether the private variant matches.
func (w Command) Auth() (AuthCommand, bool) { v, ok := w.value.(AuthCommand); return v, ok }

// Epoch returns a payload copy and whether the private variant matches.
func (w Command) Epoch() (EpochCommand, bool) { v, ok := w.value.(EpochCommand); return v, ok }

// Health returns a payload copy and whether the private variant matches.
func (w Command) Health() (Health4, bool) { v, ok := w.value.(Health4); return v, ok }

// Auth returns a payload copy and whether the private variant matches.
func (w ACK) Auth() (AuthAck, bool) { v, ok := w.value.(AuthAck); return v, ok }

// Epoch returns a payload copy and whether the private variant matches.
func (w ACK) Epoch() (EpochAck, bool) { v, ok := w.value.(EpochAck); return v, ok }

// Health returns a payload copy and whether the private variant matches.
func (w ACK) Health() (HealthAck4, bool) { v, ok := w.value.(HealthAck4); return v, ok }

// ConnectionRegister returns a payload copy and whether the private variant matches.
func (w Request) ConnectionRegister() (ConnectionRegister, bool) {
	v, ok := w.value.(ConnectionRegister)
	return v, ok
}

// ConnectionRelease returns a payload copy and whether the private variant matches.
func (w Request) ConnectionRelease() (ConnectionRelease, bool) {
	v, ok := w.value.(ConnectionRelease)
	return v, ok
}

// ProvisionalClose returns a payload copy and whether the private variant matches.
func (w Request) ProvisionalClose() (ProvisionalClose, bool) {
	v, ok := w.value.(ProvisionalClose)
	return v, ok
}

// RoomRegister returns a payload copy and whether the private variant matches.
func (w Request) RoomRegister() (RoomRegister, bool) { v, ok := w.value.(RoomRegister); return v, ok }

// RoomRelease returns a payload copy and whether the private variant matches.
func (w Request) RoomRelease() (RoomRelease, bool) { v, ok := w.value.(RoomRelease); return v, ok }

// HealthBootstrap returns a payload copy and whether the private variant matches.
func (w Request) HealthBootstrap() (HealthBootstrap, bool) {
	v, ok := w.value.(HealthBootstrap)
	return v, ok
}

// BootRetire returns a payload copy and whether the private variant matches.
func (w Request) BootRetire() (BootRetire, bool) { v, ok := w.value.(BootRetire); return v, ok }

// Registration returns a payload copy and whether the private variant matches.
func (w Response) Registration() (RegistrationResult, bool) {
	v, ok := w.value.(RegistrationResult)
	return v, ok
}

// ProvisionalClose returns a payload copy and whether the private variant matches.
func (w Response) ProvisionalClose() (ProvisionalCloseResult, bool) {
	v, ok := w.value.(ProvisionalCloseResult)
	if ok && v.Binding != nil {
		b := *v.Binding
		v.Binding = &b
	}
	return v, ok
}

// RoomRegistration returns a payload copy and whether the private variant matches.
func (w Response) RoomRegistration() (RoomRegistrationResult, bool) {
	v, ok := w.value.(RoomRegistrationResult)
	return v, ok
}

// HealthBootstrap returns a payload copy and whether the private variant matches.
func (w Response) HealthBootstrap() (HealthBootstrapResult, bool) {
	v, ok := w.value.(HealthBootstrapResult)
	return v, ok
}
