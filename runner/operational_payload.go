package runner

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"

	"github.com/RenseiAI/donmai/agent"
	"github.com/RenseiAI/donmai/executioncell"
	"github.com/RenseiAI/donmai/prompt"
	"github.com/RenseiAI/donmai/runtime/workarea"
)

// OperationalPayload is the one shared, lossless admission-time projection of
// QueuedWork. It contains every upstream operational field the runner consumes
// and deliberately excludes only execution-contract sidecars and daemon-local
// runtime annotations (worker credentials, worker id, and daemon capabilities).
// Adding an operational field to QueuedWork requires adding it here so producer
// and verifier digests cannot drift silently.
type OperationalPayload struct {
	prompt.QueuedWork

	RepositoryDeclaration *workarea.RepositoryDeclarationV1 `json:"repositoryDeclaration,omitempty"`
	WorkareaMode          string                            `json:"workareaMode,omitempty"`
	ParentWorkareaID      string                            `json:"parentWorkareaId,omitempty"`
	RepositoryFilter      *workarea.RepositoryFilter        `json:"repositoryFilter,omitempty"`
	CacheSeedID           string                            `json:"cacheSeedId,omitempty"`
	ResolvedProfile       ResolvedProfile                   `json:"resolvedProfile,omitempty"`
	Branch                string                            `json:"branch,omitempty"`
	TerminalWorkareaLease *workarea.TerminalLeaseRequest    `json:"terminalWorkareaLease,omitempty"`
	PermissionProfile     PermissionProfile                 `json:"permissionProfile,omitempty"`
}

// ProjectOperationalPayload returns the exact admission-time payload shared by
// receipt producers and the runner verifier. It is never persisted as evidence;
// only its canonical digest belongs in an AdmissionReceipt.
func ProjectOperationalPayload(qw QueuedWork) OperationalPayload {
	return OperationalPayload{
		QueuedWork:            qw.QueuedWork,
		RepositoryDeclaration: qw.RepositoryDeclaration,
		WorkareaMode:          qw.WorkareaMode,
		ParentWorkareaID:      qw.ParentWorkareaID,
		RepositoryFilter:      qw.RepositoryFilter,
		CacheSeedID:           qw.CacheSeedID,
		ResolvedProfile:       qw.ResolvedProfile,
		Branch:                qw.Branch,
		TerminalWorkareaLease: qw.TerminalWorkareaLease,
		PermissionProfile:     qw.PermissionProfile,
	}
}

var (
	rawMessageType      = reflect.TypeOf(json.RawMessage{})
	endpointBindingType = reflect.TypeOf(agent.EndpointBinding{})
)

// CanonicalOperationalPayload returns the shared RFC 8785 bytes hashed by
// admission producers and consumers. Unlike encoding/json's omitempty rule,
// this projection retains non-nil empty slices and maps, recursively. That is
// the only way a decoded Go payload can preserve the source contract's
// meaningful absent-versus-present-empty distinction.
func CanonicalOperationalPayload(qw QueuedWork) ([]byte, error) {
	if len(qw.OperationalPayload) > 0 {
		return executioncell.NormalizeOperationalPayload(qw.OperationalPayload)
	}
	document, err := losslessOperationalValue(reflect.ValueOf(ProjectOperationalPayload(qw)))
	if err != nil {
		return nil, err
	}
	return executioncell.CanonicalJSON(document)
}

func losslessOperationalValue(value reflect.Value) (any, error) {
	if !value.IsValid() {
		return nil, nil
	}
	if value.Kind() == reflect.Interface {
		if value.IsNil() {
			return nil, nil
		}
		return losslessOperationalValue(value.Elem())
	}
	if value.Kind() == reflect.Pointer {
		if value.IsNil() {
			return nil, nil
		}
		return losslessOperationalValue(value.Elem())
	}
	if value.Type() == rawMessageType {
		if value.IsNil() {
			return nil, nil
		}
		var document any
		if err := json.Unmarshal(value.Bytes(), &document); err != nil {
			return nil, fmt.Errorf("runner: decode operational raw JSON: %w", err)
		}
		return document, nil
	}
	if value.Type() == endpointBindingType {
		endpoint := value.Interface().(agent.EndpointBinding)
		// Delivery material is resolved after admission (for example a
		// worker-local gateway URL and bearer). Bind only stable cell identity
		// here; the complete effective-cell/live-binding comparison below owns
		// the execution axes and forbids ambient substitution.
		return map[string]any{
			"company": endpoint.Company, "model": endpoint.Model, "protocol": endpoint.Protocol, "host": endpoint.Host,
			"endpointId": endpoint.EndpointID, "endpointOperator": endpoint.EndpointOperator, "endpointRevision": endpoint.EndpointRevision,
			"modelAuthor": endpoint.ModelAuthor, "authBindingId": endpoint.AuthBindingID, "authAuthority": endpoint.AuthAuthority,
			"authCommercialMode": endpoint.AuthCommercialMode, "authBindingScope": endpoint.AuthBindingScope,
			"authPortability": endpoint.AuthPortability, "authDelivery": endpoint.AuthDelivery, "mechanism": endpoint.Mechanism,
		}, nil
	}

	switch value.Kind() {
	case reflect.Struct:
		document := make(map[string]any)
		typeOfValue := value.Type()
		for index := 0; index < value.NumField(); index++ {
			fieldInfo := typeOfValue.Field(index)
			if fieldInfo.PkgPath != "" {
				continue
			}
			name, omitEmpty, skip := operationalJSONField(fieldInfo)
			if skip {
				continue
			}
			fieldValue := value.Field(index)
			if fieldInfo.Anonymous && name == "" {
				embedded, err := losslessOperationalValue(fieldValue)
				if err != nil {
					return nil, err
				}
				fields, ok := embedded.(map[string]any)
				if !ok {
					return nil, fmt.Errorf("runner: embedded operational field %s is not an object", fieldInfo.Name)
				}
				for key, child := range fields {
					document[key] = child
				}
				continue
			}
			if omitEmpty && operationalOmit(fieldValue) {
				continue
			}
			child, err := losslessOperationalValue(fieldValue)
			if err != nil {
				return nil, err
			}
			document[name] = child
		}
		return document, nil
	case reflect.Slice, reflect.Array:
		if value.Kind() == reflect.Slice && value.IsNil() {
			return nil, nil
		}
		children := make([]any, value.Len())
		for index := 0; index < value.Len(); index++ {
			child, err := losslessOperationalValue(value.Index(index))
			if err != nil {
				return nil, err
			}
			children[index] = child
		}
		return children, nil
	case reflect.Map:
		if value.IsNil() {
			return nil, nil
		}
		if value.Type().Key().Kind() != reflect.String {
			return nil, fmt.Errorf("runner: operational map key %s is not a string", value.Type().Key())
		}
		document := make(map[string]any, value.Len())
		iterator := value.MapRange()
		for iterator.Next() {
			child, err := losslessOperationalValue(iterator.Value())
			if err != nil {
				return nil, err
			}
			document[iterator.Key().String()] = child
		}
		return document, nil
	case reflect.Bool:
		return value.Bool(), nil
	case reflect.String:
		return value.String(), nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return value.Int(), nil
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return value.Uint(), nil
	case reflect.Float32, reflect.Float64:
		return value.Float(), nil
	default:
		return nil, fmt.Errorf("runner: unsupported operational field kind %s", value.Kind())
	}
}

func operationalJSONField(field reflect.StructField) (name string, omitEmpty, skip bool) {
	tag := field.Tag.Get("json")
	if tag == "-" {
		return "", false, true
	}
	parts := strings.Split(tag, ",")
	name = parts[0]
	if name == "" && !field.Anonymous {
		name = field.Name
	}
	for _, option := range parts[1:] {
		omitEmpty = omitEmpty || option == "omitempty"
	}
	return name, omitEmpty, false
}

func operationalOmit(value reflect.Value) bool {
	switch value.Kind() {
	case reflect.Interface, reflect.Pointer, reflect.Map, reflect.Slice:
		// A non-nil empty map/slice is present-empty and must remain present.
		return value.IsNil()
	case reflect.Bool:
		return !value.Bool()
	case reflect.String:
		return value.Len() == 0
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return value.Int() == 0
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return value.Uint() == 0
	case reflect.Float32, reflect.Float64:
		return value.Float() == 0
	default:
		return false
	}
}

// DigestOperationalPayload returns the RFC 8785 SHA-256 digest of the shared
// lossless projection. Secret-bearing MCP inputs may contribute to the digest,
// but their values are never copied into receipts or errors.
func DigestOperationalPayload(qw QueuedWork) (string, error) {
	canonical, err := CanonicalOperationalPayload(qw)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(canonical)
	return hex.EncodeToString(digest[:]), nil
}
