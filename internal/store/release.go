package store

import (
	"bytes"
	"cmp"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"

	helmv2 "github.com/fluxcd/helm-controller/api/v2"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	"github.com/mycroft/fluxcd-ui/internal/flux"
)

const (
	// helmReleaseType is the type of the Secrets Helm stores releases in.
	helmReleaseType corev1.SecretType = "helm.sh/release.v1"
	// maxReleaseSize caps a decompressed release. Secrets hold at most 1 MiB,
	// so a larger release would be a decompression bomb.
	maxReleaseSize = 64 << 20
)

var (
	// ErrReleaseContentUnavailable is returned when the store cannot read
	// Secrets.
	ErrReleaseContentUnavailable = errors.New("the Helm release content is not available")
	// ErrNoRelease is returned for a HelmRelease that has no Helm release yet.
	ErrNoRelease = errors.New("this HelmRelease has no Helm release yet")
)

// ReleaseContent is what Helm stored for one release revision.
type ReleaseContent struct {
	Name      string
	Namespace string // where Helm stores the release
	Revision  int
	Chart     string // name@version
	Status    string // deployed, superseded, failed, ...
	// Values are the values Flux passed to Helm (spec.values merged with
	// spec.valuesFrom), as YAML. The chart's own defaults are not included.
	Values   string
	Manifest string // rendered templates, without hooks
}

// helmRelease is the part of Helm's stored release we use.
type helmRelease struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	Version   int    `json:"version"`
	Info      struct {
		Status string `json:"status"`
	} `json:"info"`
	Chart struct {
		Metadata struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"metadata"`
	} `json:"chart"`
	// Config stays JSON until it is shown: decoding it into map[string]any
	// would round integers above 2^53.
	Config   json.RawMessage `json:"config"`
	Manifest string          `json:"manifest"`
}

// HelmReleaseContent returns the values and manifest of the current Helm
// release of a HelmRelease, read from the Secret Helm stores it in.
func (s *Store) HelmReleaseContent(ctx context.Context, namespace, name string) (*ReleaseContent, error) {
	if s.clientset == nil {
		return nil, ErrReleaseContentUnavailable
	}
	hr := &helmv2.HelmRelease{}
	if err := s.reader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, hr); err != nil {
		return nil, err
	}
	latest := flux.LatestSnapshot(hr.Status.History)
	if latest == nil {
		return nil, ErrNoRelease
	}

	// helm-controller records where it stored the release. spec.storageNamespace
	// would do too, but anyone allowed to edit the HelmRelease can point it at
	// another namespace, while status.history still names their own release.
	storage := cmp.Or(hr.Status.StorageNamespace, hr.GetStorageNamespace())
	secretName := "sh.helm.release.v1." + latest.Name + ".v" + strconv.Itoa(latest.Version)
	secret, err := s.clientset.CoreV1().Secrets(storage).Get(ctx, secretName, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("reading Secret %s/%s: %w", storage, secretName, err)
	}
	// Never show a Secret that is not a Helm release.
	if secret.Type != helmReleaseType {
		return nil, fmt.Errorf("the Secret %s/%s is of type %q, not a Helm release", storage, secretName, secret.Type)
	}
	rel, err := decodeRelease(secret.Data["release"])
	if err != nil {
		return nil, fmt.Errorf("decoding Secret %s/%s: %w", storage, secretName, err)
	}
	// Nor one holding another release than the HelmRelease's current one.
	if rel.Name != latest.Name || rel.Namespace != latest.Namespace || rel.Version != latest.Version {
		return nil, fmt.Errorf("the Secret %s/%s holds release %s/%s v%d, not %s/%s v%d",
			storage, secretName, rel.Namespace, rel.Name, rel.Version, latest.Namespace, latest.Name, latest.Version)
	}

	c := &ReleaseContent{
		Name:      latest.Name,
		Namespace: storage,
		Revision:  rel.Version,
		Chart:     rel.Chart.Metadata.Name + "@" + rel.Chart.Metadata.Version,
		Status:    rel.Info.Status,
		Manifest:  rel.Manifest,
	}
	if len(rel.Config) > 0 {
		values, err := yaml.JSONToYAML(rel.Config) // keeps integers exact
		if err != nil {
			return nil, fmt.Errorf("encoding values: %w", err)
		}
		if v := string(values); v != "{}\n" && v != "null\n" {
			c.Values = v
		}
	}
	return c, nil
}

// decodeRelease decodes a release the way Helm's Secret driver stores it:
// JSON, gzipped, then base64-encoded (on top of the Secret's own encoding).
func decodeRelease(data []byte) (*helmRelease, error) {
	raw, err := base64.StdEncoding.DecodeString(string(data))
	if err != nil {
		return nil, err
	}
	// Helm reads uncompressed releases too.
	if bytes.HasPrefix(raw, []byte{0x1f, 0x8b, 0x08}) {
		zr, err := gzip.NewReader(bytes.NewReader(raw))
		if err != nil {
			return nil, err
		}
		defer func() { _ = zr.Close() }()
		if raw, err = io.ReadAll(io.LimitReader(zr, maxReleaseSize+1)); err != nil {
			return nil, err
		}
		if len(raw) > maxReleaseSize {
			return nil, fmt.Errorf("the release is larger than %d MiB", maxReleaseSize>>20)
		}
	}
	var rel helmRelease
	if err := json.Unmarshal(raw, &rel); err != nil {
		return nil, err
	}
	return &rel, nil
}
