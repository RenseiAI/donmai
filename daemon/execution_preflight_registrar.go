package daemon

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/RenseiAI/donmai/executioncell"
)

// ExecutionPreflightRegistrar is selected from trusted daemon/controller
// configuration. Work items and callback URLs never select it.
type ExecutionPreflightRegistrar interface {
	RegisterExecutionPreflight(context.Context, executioncell.PreflightRegistrationRequest) (executioncell.PreflightRegistrationResponse, error)
}

// localExecutionPreflightAuthority is the OSS controller-side authority used
// by the daemon. The admission is recorded only after the daemon validates the
// actual admission/claim/runtime binding. Registration consumes that durable
// pre-start fact; it cannot mint permission from a host receipt alone.
type localExecutionPreflightAuthority interface {
	admitExecutionPreflight(context.Context, localExecutionPreflightAdmission) error
	beginExecutionPreflightStart(context.Context, executioncell.PreflightRegistrationRequest, executioncell.PreflightRegistrationResponse) error
	retireExecutionPreflight(context.Context, string) error
}

type localExecutionPreflightAdmission struct {
	ContractVersion          string                       `json:"contractVersion"`
	RuntimeBinding           executioncell.RuntimeBinding `json:"runtimeBinding"`
	OperationalPayloadDigest string                       `json:"operationalPayloadDigest"`
	AdmissionReceiptSHA256   string                       `json:"admissionReceiptSha256"`
	ClaimReceiptSHA256       string                       `json:"claimReceiptSha256,omitempty"`
	EffectiveCellSHA256      string                       `json:"effectiveCellSha256"`
	AuthorizationRevision    uint64                       `json:"authorizationRevision"`
}

type localExecutionPreflightStart struct {
	ContractVersion string                       `json:"contractVersion"`
	RegistrationID  string                       `json:"registrationId"`
	ReceiptSHA256   string                       `json:"receiptSha256"`
	RuntimeBinding  executioncell.RuntimeBinding `json:"runtimeBinding"`
}

type localExecutionPreflightRetirement struct {
	ContractVersion string `json:"contractVersion"`
	RequestID       string `json:"requestId"`
}

const localExecutionPreflightAuthorityVersion = "execution-preflight-local-authority/v1"

func persistExecutionPreflightV2(store ExecutionPreflightStore, sessionID string, receipt json.RawMessage) (json.RawMessage, error) {
	if len(receipt) == 0 || len(receipt) > executioncell.MaxPreflightRegistrationReceiptBytes {
		return nil, errors.New("runtime binding v2 execution preflight receipt size is invalid")
	}
	replayable, ok := store.(ExecutionPreflightReplayStore)
	if !ok {
		return nil, errors.New("runtime binding v2 requires an exact-replay execution preflight store")
	}
	if err := store.Persist(sessionID, receipt); err == nil {
		return bytes.Clone(receipt), nil
	} else if !errors.Is(err, os.ErrExist) {
		return nil, fmt.Errorf("persist execution preflight receipt: %w", err)
	}
	existing, err := replayable.Load(sessionID)
	if err != nil {
		return nil, fmt.Errorf("recover fsynced execution preflight receipt: %w", err)
	}
	if !bytes.Equal(existing, receipt) {
		return nil, errors.New("execution preflight receipt changed after fsync")
	}
	return existing, nil
}

// HTTPExecutionPreflightRegistrarOptions configure the generic authenticated
// controller client. Token is read per call so rotation does not strand a
// long-lived daemon.
type HTTPExecutionPreflightRegistrarOptions struct {
	BaseURL string
	Client  *http.Client
	Token   func() string
}

// HTTPExecutionPreflightRegistrar posts registration to one fixed trusted
// controller origin and refuses redirects.
type HTTPExecutionPreflightRegistrar struct {
	base   *url.URL
	client *http.Client
	token  func() string
}

// NewHTTPExecutionPreflightRegistrar validates and freezes the controller
// origin while retaining a dynamic token source for rotation.
func NewHTTPExecutionPreflightRegistrar(options HTTPExecutionPreflightRegistrarOptions) (*HTTPExecutionPreflightRegistrar, error) {
	base, err := url.Parse(strings.TrimSpace(options.BaseURL))
	if err != nil || base.Scheme == "" || base.Host == "" || base.User != nil || base.RawQuery != "" || base.Fragment != "" || (base.Path != "" && base.Path != "/") {
		return nil, errors.New("execution preflight registrar requires a fixed absolute origin")
	}
	host := base.Hostname()
	loopback := host == "localhost" || net.ParseIP(host).IsLoopback()
	if base.Scheme != "https" && !(base.Scheme == "http" && loopback) {
		return nil, errors.New("execution preflight registrar requires HTTPS or loopback HTTP")
	}
	if options.Token == nil {
		return nil, errors.New("execution preflight registrar token source is required")
	}
	client := options.Client
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	clientCopy := *client
	clientCopy.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	}
	base.Path, base.RawPath = "", ""
	return &HTTPExecutionPreflightRegistrar{base: base, client: &clientCopy, token: options.Token}, nil
}

// RegisterExecutionPreflight posts one bounded closed request and decodes one
// bounded closed response over the configured authenticated channel.
func (r *HTTPExecutionPreflightRegistrar) RegisterExecutionPreflight(ctx context.Context, request executioncell.PreflightRegistrationRequest) (executioncell.PreflightRegistrationResponse, error) {
	if r == nil || r.base == nil || r.client == nil || r.token == nil {
		return executioncell.PreflightRegistrationResponse{}, errors.New("execution preflight HTTP registrar is not configured")
	}
	if err := executioncell.ValidatePreflightRegistrationRequest(request); err != nil {
		return executioncell.PreflightRegistrationResponse{}, err
	}
	requestContext, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	token := strings.TrimSpace(r.token())
	if token == "" {
		return executioncell.PreflightRegistrationResponse{}, errors.New("execution preflight registrar authentication is unavailable")
	}
	body, err := json.Marshal(request)
	if err != nil {
		return executioncell.PreflightRegistrationResponse{}, err
	}
	endpoint := *r.base
	endpoint.Path = strings.TrimSuffix(endpoint.Path, "/") + "/api/daemon/sessions/" + url.PathEscape(request.RuntimeBinding.RequestID) + "/execution-preflight-registration"
	httpRequest, err := http.NewRequestWithContext(requestContext, http.MethodPost, endpoint.String(), bytes.NewReader(body))
	if err != nil {
		return executioncell.PreflightRegistrationResponse{}, err
	}
	httpRequest.Header.Set("Authorization", "Bearer "+token)
	httpRequest.Header.Set("Content-Type", "application/json")
	httpRequest.Header.Set("Accept", "application/json")
	response, err := r.client.Do(httpRequest)
	if err != nil {
		return executioncell.PreflightRegistrationResponse{}, fmt.Errorf("register execution preflight: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	raw, readErr := io.ReadAll(io.LimitReader(response.Body, 64*1024+1))
	if readErr != nil {
		return executioncell.PreflightRegistrationResponse{}, fmt.Errorf("read execution preflight registration response: %w", readErr)
	}
	if len(raw) > 64*1024 {
		return executioncell.PreflightRegistrationResponse{}, errors.New("execution preflight registration response is too large")
	}
	if response.StatusCode != http.StatusOK {
		return executioncell.PreflightRegistrationResponse{}, fmt.Errorf("execution preflight registration returned HTTP %d", response.StatusCode)
	}
	decoded, err := executioncell.DecodePreflightRegistrationResponse(raw)
	if err != nil {
		return executioncell.PreflightRegistrationResponse{}, err
	}
	return decoded, nil
}

// FileExecutionPreflightRegistrar is the OSS local-controller implementation.
// The daemon calls it only after authenticating/validating the admitted detail;
// it durably binds that exact request and returns the same acknowledgement on
// replay. It contains no hosted control-plane assumptions.
type FileExecutionPreflightRegistrar struct {
	dir string
	mu  sync.Mutex
}

// NewFileExecutionPreflightRegistrar constructs the OSS local-controller
// registrar rooted at an append-only directory.
func NewFileExecutionPreflightRegistrar(dir string) *FileExecutionPreflightRegistrar {
	return &FileExecutionPreflightRegistrar{dir: dir}
}

type filePreflightRegistrationRecord struct {
	Request  executioncell.PreflightRegistrationRequest  `json:"request"`
	Response executioncell.PreflightRegistrationResponse `json:"response"`
}

func localExecutionPreflightRecordBase(requestID string) string {
	digest := sha256.Sum256([]byte(requestID))
	return hex.EncodeToString(digest[:])
}

func registrationRecordName(request executioncell.PreflightRegistrationRequest) string {
	return localExecutionPreflightRecordBase(request.RuntimeBinding.RequestID) + ".registration.json"
}

func localRegistrationResponse(request executioncell.PreflightRegistrationRequest) executioncell.PreflightRegistrationResponse {
	raw, _ := json.Marshal(request)
	digest := sha256.Sum256(raw)
	binding := request.RuntimeBinding
	return executioncell.PreflightRegistrationResponse{
		ContractVersion: executioncell.PreflightRegistrationContractVersion,
		Decision:        "authorized",
		RegistrationID:  "preflight_registration_" + hex.EncodeToString(digest[:16]),
		ReceiptSHA256:   request.ReceiptSHA256,
		RuntimeBinding:  &binding,
		// Revision 1 is the durable admitted-session authority; revision 2 is
		// the exact registration transition committed before this response.
		AuthorizationRevision: 2,
	}
}

func localAdmissionRecordName(requestID string) string {
	return localExecutionPreflightRecordBase(requestID) + ".admission.json"
}

func localStartRecordName(requestID string) string {
	return localExecutionPreflightRecordBase(requestID) + ".started.json"
}

func localRetirementRecordName(requestID string) string {
	return localExecutionPreflightRecordBase(requestID) + ".retired.json"
}

func digestPreflightBytes(raw []byte) string {
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:])
}

func validPreflightDigest(value string) bool {
	if len(value) != sha256.Size*2 || value != strings.ToLower(value) {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func durableLocalPreflightRecord(root *os.Root, name string, raw []byte) error {
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return fmt.Errorf("generate local execution preflight nonce: %w", err)
	}
	pending := "." + name + fmt.Sprintf(".%x.pending", nonce)
	file, err := root.OpenFile(pending, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer func() { _ = root.Remove(pending) }()
	if _, err = file.Write(raw); err == nil {
		err = file.Sync()
	}
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err = root.Link(pending, name); err != nil {
		return err
	}
	return syncRegistrationRoot(root)
}

func readLocalPreflightRecord(root *os.Root, name string) ([]byte, error) {
	file, err := root.Open(name)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	raw, err := io.ReadAll(io.LimitReader(file, 512*1024+1))
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 || len(raw) > 512*1024 {
		return nil, errors.New("local execution preflight record size is invalid")
	}
	return raw, nil
}

func localPreflightRecordExists(root *os.Root, name string) (bool, error) {
	file, err := root.Open(name)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	if err := file.Close(); err != nil {
		return false, err
	}
	return true, nil
}

func openLocalPreflightRoot(dir string) (*os.Root, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, errors.New("execution preflight registration directory is required")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create execution preflight registration directory: %w", err)
	}
	return os.OpenRoot(dir)
}

// admitExecutionPreflight durably records the local controller's actual
// admitted, pre-start authority. Exact retries are allowed; changed authority,
// a consumed start, or a retired lifecycle refuse before compilation.
func (r *FileExecutionPreflightRegistrar) admitExecutionPreflight(ctx context.Context, admission localExecutionPreflightAdmission) error {
	if r == nil {
		return errors.New("execution preflight local authority is not configured")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if admission.ContractVersion != localExecutionPreflightAuthorityVersion || admission.AuthorizationRevision != 1 ||
		!validPreflightDigest(admission.OperationalPayloadDigest) || !validPreflightDigest(admission.AdmissionReceiptSHA256) ||
		!validPreflightDigest(admission.EffectiveCellSHA256) ||
		(admission.RuntimeBinding.ClaimID == "" && admission.ClaimReceiptSHA256 != "") ||
		(admission.RuntimeBinding.ClaimID != "" && !validPreflightDigest(admission.ClaimReceiptSHA256)) {
		return errors.New("execution preflight local admission is invalid")
	}
	bindingRaw, err := json.Marshal(admission.RuntimeBinding)
	if err != nil {
		return err
	}
	if _, err := executioncell.DecodeRuntimeBinding(bindingRaw); err != nil || admission.RuntimeBinding.ContractVersion != executioncell.RuntimeBindingV2ContractVersion {
		return errors.New("execution preflight local admission requires runtime binding v2")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	root, err := openLocalPreflightRoot(r.dir)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	requestID := admission.RuntimeBinding.RequestID
	if retired, checkErr := localPreflightRecordExists(root, localRetirementRecordName(requestID)); checkErr != nil {
		return checkErr
	} else if retired {
		return errors.New("execution preflight claim is retired")
	}
	if started, checkErr := localPreflightRecordExists(root, localStartRecordName(requestID)); checkErr != nil {
		return checkErr
	} else if started {
		return errors.New("execution preflight already started")
	}
	raw, err := json.Marshal(admission)
	if err != nil {
		return err
	}
	name := localAdmissionRecordName(requestID)
	if existing, readErr := readLocalPreflightRecord(root, name); readErr == nil {
		if !bytes.Equal(existing, raw) {
			return errors.New("execution preflight local admission changed")
		}
		return syncRegistrationRoot(root)
	} else if !errors.Is(readErr, os.ErrNotExist) {
		return readErr
	}
	return durableLocalPreflightRecord(root, name, raw)
}

// RegisterExecutionPreflight stores one exact local authority preimage and
// returns its deterministic acknowledgement; exact replay returns the same.
func (r *FileExecutionPreflightRegistrar) RegisterExecutionPreflight(ctx context.Context, request executioncell.PreflightRegistrationRequest) (executioncell.PreflightRegistrationResponse, error) {
	if r == nil || strings.TrimSpace(r.dir) == "" {
		return executioncell.PreflightRegistrationResponse{}, errors.New("execution preflight registration directory is required")
	}
	if err := executioncell.ValidatePreflightRegistrationRequest(request); err != nil {
		return executioncell.PreflightRegistrationResponse{}, err
	}
	if err := ctx.Err(); err != nil {
		return executioncell.PreflightRegistrationResponse{}, err
	}
	receipt, err := request.ReceiptBytes()
	if err != nil {
		return executioncell.PreflightRegistrationResponse{}, err
	}
	hostReceipt, err := executioncell.DecodeHostAdaptationReceipt(receipt)
	if err != nil {
		return executioncell.PreflightRegistrationResponse{}, err
	}
	if hostReceipt.Decision != "ready" {
		return executioncell.PreflightRegistrationResponse{ContractVersion: executioncell.PreflightRegistrationContractVersion, Decision: "refused", Code: executioncell.PreflightReceiptDenied}, nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	root, err := openLocalPreflightRoot(r.dir)
	if err != nil {
		return executioncell.PreflightRegistrationResponse{}, err
	}
	defer func() { _ = root.Close() }()
	requestID := request.RuntimeBinding.RequestID
	if retired, checkErr := localPreflightRecordExists(root, localRetirementRecordName(requestID)); checkErr != nil {
		return executioncell.PreflightRegistrationResponse{}, checkErr
	} else if retired {
		return executioncell.PreflightRegistrationResponse{ContractVersion: executioncell.PreflightRegistrationContractVersion, Decision: "refused", Code: executioncell.PreflightClaimRetired}, nil
	}
	if started, checkErr := localPreflightRecordExists(root, localStartRecordName(requestID)); checkErr != nil {
		return executioncell.PreflightRegistrationResponse{}, checkErr
	} else if started {
		return executioncell.PreflightRegistrationResponse{ContractVersion: executioncell.PreflightRegistrationContractVersion, Decision: "refused", Code: executioncell.PreflightAlreadyStarted}, nil
	}
	admissionRaw, readErr := readLocalPreflightRecord(root, localAdmissionRecordName(requestID))
	if errors.Is(readErr, os.ErrNotExist) {
		return executioncell.PreflightRegistrationResponse{ContractVersion: executioncell.PreflightRegistrationContractVersion, Decision: "refused", Code: executioncell.PreflightAuthorizationUnavailable}, nil
	}
	if readErr != nil {
		return executioncell.PreflightRegistrationResponse{}, readErr
	}
	var admission localExecutionPreflightAdmission
	if err := json.Unmarshal(admissionRaw, &admission); err != nil {
		return executioncell.PreflightRegistrationResponse{}, errors.New("execution preflight local admission is corrupt")
	}
	canonicalAdmission, err := json.Marshal(admission)
	if err != nil || !bytes.Equal(canonicalAdmission, admissionRaw) || admission.ContractVersion != localExecutionPreflightAuthorityVersion || admission.AuthorizationRevision != 1 {
		return executioncell.PreflightRegistrationResponse{}, errors.New("execution preflight local admission is corrupt")
	}
	admittedBinding, _ := json.Marshal(admission.RuntimeBinding)
	requestBinding, _ := json.Marshal(request.RuntimeBinding)
	if !bytes.Equal(admittedBinding, requestBinding) || admission.OperationalPayloadDigest != request.OperationalPayloadDigest {
		return executioncell.PreflightRegistrationResponse{ContractVersion: executioncell.PreflightRegistrationContractVersion, Decision: "refused", Code: executioncell.PreflightBindingMismatch}, nil
	}
	// Reaffirm the admission directory entry before consuming it as current
	// authorization, including crash recovery after link-before-dirsync.
	if err := syncRegistrationRoot(root); err != nil {
		return executioncell.PreflightRegistrationResponse{}, err
	}
	response := localRegistrationResponse(request)
	record := filePreflightRegistrationRecord{Request: request, Response: response}
	raw, err := json.Marshal(record)
	if err != nil {
		return executioncell.PreflightRegistrationResponse{}, err
	}
	name := registrationRecordName(request)
	if existing, readErr := readLocalPreflightRecord(root, name); readErr == nil {
		if !bytes.Equal(existing, raw) {
			return executioncell.PreflightRegistrationResponse{ContractVersion: executioncell.PreflightRegistrationContractVersion, Decision: "refused", Code: executioncell.PreflightBindingMismatch}, nil
		}
		if syncErr := syncRegistrationRoot(root); syncErr != nil {
			return executioncell.PreflightRegistrationResponse{}, syncErr
		}
		return response, nil
	} else if !errors.Is(readErr, os.ErrNotExist) {
		return executioncell.PreflightRegistrationResponse{}, readErr
	}
	if err = durableLocalPreflightRecord(root, name, raw); err != nil {
		if errors.Is(err, os.ErrExist) {
			existing, readErr := readLocalPreflightRecord(root, name)
			if readErr == nil && bytes.Equal(existing, raw) {
				if syncErr := syncRegistrationRoot(root); syncErr != nil {
					return executioncell.PreflightRegistrationResponse{}, syncErr
				}
				return response, nil
			}
			return executioncell.PreflightRegistrationResponse{ContractVersion: executioncell.PreflightRegistrationContractVersion, Decision: "refused", Code: executioncell.PreflightBindingMismatch}, nil
		}
		return executioncell.PreflightRegistrationResponse{}, err
	}
	return response, nil
}

// beginExecutionPreflightStart consumes one registered pre-start authority
// before any credential hook or process spawn. The marker is append-only, so a
// crash cannot reopen the start permission.
func (r *FileExecutionPreflightRegistrar) beginExecutionPreflightStart(ctx context.Context, request executioncell.PreflightRegistrationRequest, response executioncell.PreflightRegistrationResponse) error {
	if r == nil {
		return errors.New("execution preflight local authority is not configured")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := executioncell.ValidateAuthorizedPreflightRegistration(request, response); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	root, err := openLocalPreflightRoot(r.dir)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	requestID := request.RuntimeBinding.RequestID
	if retired, checkErr := localPreflightRecordExists(root, localRetirementRecordName(requestID)); checkErr != nil {
		return checkErr
	} else if retired {
		return errors.New("execution preflight claim is retired")
	}
	if started, checkErr := localPreflightRecordExists(root, localStartRecordName(requestID)); checkErr != nil {
		return checkErr
	} else if started {
		return errors.New("execution preflight already started")
	}
	recordRaw, err := readLocalPreflightRecord(root, registrationRecordName(request))
	if err != nil {
		return fmt.Errorf("read execution preflight registration before start: %w", err)
	}
	expectedRaw, err := json.Marshal(filePreflightRegistrationRecord{Request: request, Response: response})
	if err != nil || !bytes.Equal(recordRaw, expectedRaw) {
		return errors.New("execution preflight registration changed before start")
	}
	start := localExecutionPreflightStart{ContractVersion: localExecutionPreflightAuthorityVersion, RegistrationID: response.RegistrationID, ReceiptSHA256: response.ReceiptSHA256, RuntimeBinding: request.RuntimeBinding}
	raw, err := json.Marshal(start)
	if err != nil {
		return err
	}
	return durableLocalPreflightRecord(root, localStartRecordName(requestID), raw)
}

// retireExecutionPreflight appends the local terminal/failed-start fact. A
// retained start marker remains visible, but retirement takes precedence on
// every future registration attempt.
func (r *FileExecutionPreflightRegistrar) retireExecutionPreflight(ctx context.Context, requestID string) error {
	if r == nil || strings.TrimSpace(requestID) == "" {
		return errors.New("execution preflight local retirement is invalid")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	root, err := openLocalPreflightRoot(r.dir)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	if _, err := readLocalPreflightRecord(root, localAdmissionRecordName(requestID)); err != nil {
		return fmt.Errorf("retire unknown execution preflight admission: %w", err)
	}
	name := localRetirementRecordName(requestID)
	record := localExecutionPreflightRetirement{ContractVersion: localExecutionPreflightAuthorityVersion, RequestID: requestID}
	raw, err := json.Marshal(record)
	if err != nil {
		return err
	}
	if existing, readErr := readLocalPreflightRecord(root, name); readErr == nil {
		if !bytes.Equal(existing, raw) {
			return errors.New("execution preflight retirement changed")
		}
		return syncRegistrationRoot(root)
	} else if !errors.Is(readErr, os.ErrNotExist) {
		return readErr
	}
	return durableLocalPreflightRecord(root, name, raw)
}

func syncRegistrationRoot(root *os.Root) error {
	directory, err := root.Open(".")
	if err != nil {
		return err
	}
	if err = directory.Sync(); err != nil {
		_ = directory.Close()
		return err
	}
	if err = directory.Close(); err != nil {
		return err
	}
	return nil
}
