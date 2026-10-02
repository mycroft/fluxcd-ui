// Package authz decides who may act on Flux objects.
package authz

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	authorizationv1 "k8s.io/api/authorization/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/mycroft/fluxcd-ui/internal/store"
)

// Verbs checked against Kubernetes RBAC. No API endpoint uses them, so
// granting them to people gives no direct access to the API.
const (
	VerbReconcile = "reconcile"
	VerbSuspend   = "suspend" // also covers resume
	// VerbInspect lets a user read a HelmRelease's Helm release: its values,
	// which may hold credentials, and its rendered manifest.
	VerbInspect = "inspect"
)

// Identity is a user, and their groups, as vouched for by the authenticating
// proxy in front of the UI.
type Identity struct {
	User   string
	Groups []string
}

// Authorizer decides whether an identity may apply a verb to a Flux object.
type Authorizer interface {
	// Allowed reports whether id may apply verb to obj. Answers may be cached
	// briefly, unless fresh is set.
	Allowed(ctx context.Context, id Identity, verb string, obj store.ObjectRef, fresh bool) (bool, error)
}

// AllowAll lets any identity act.
type AllowAll struct{}

func (AllowAll) Allowed(context.Context, Identity, string, store.ObjectRef, bool) (bool, error) {
	return true, nil
}

// RBAC delegates decisions to Kubernetes RBAC: it asks the API server, with
// a SubjectAccessReview, whether the identity's user and groups (with the
// prefix prepended) are granted the verb on the object.
type RBAC struct {
	client client.Client
	prefix string
	ttl    time.Duration
	now    func() time.Time

	mu    sync.Mutex
	cache map[string]cachedAnswer
}

type cachedAnswer struct {
	allowed bool
	expires time.Time
}

// maxCacheEntries bounds the answer cache; it is reset when full.
const maxCacheEntries = 10000

// ValidatePrefix rejects subject prefixes that would let proxy-provided names
// collide with Kubernetes built-in users and groups, such as system:masters.
func ValidatePrefix(prefix string) error {
	switch {
	case prefix == "":
		return errors.New("the subject prefix must not be empty")
	case strings.HasPrefix(prefix, "system:"):
		return errors.New(`the subject prefix must not start with "system:"`)
	}
	return nil
}

// NewRBAC returns an RBAC authorizer creating SubjectAccessReviews with c and
// caching answers for ttl.
func NewRBAC(c client.Client, prefix string, ttl time.Duration) (*RBAC, error) {
	if err := ValidatePrefix(prefix); err != nil {
		return nil, err
	}
	return &RBAC{client: c, prefix: prefix, ttl: ttl, now: time.Now, cache: map[string]cachedAnswer{}}, nil
}

func (a *RBAC) Allowed(ctx context.Context, id Identity, verb string, obj store.ObjectRef, fresh bool) (bool, error) {
	if id.User == "" {
		return false, nil
	}
	user := a.prefix + id.User
	groups := make([]string, len(id.Groups))
	for i, g := range id.Groups {
		groups[i] = a.prefix + g
	}
	slices.Sort(groups)
	gr := obj.GroupResource()
	key := strings.Join(append([]string{user, verb, gr.String(), obj.Namespace, obj.Name}, groups...), "\x00")

	if !fresh {
		a.mu.Lock()
		c, ok := a.cache[key]
		a.mu.Unlock()
		if ok && a.now().Before(c.expires) {
			return c.allowed, nil
		}
	}

	sar := &authorizationv1.SubjectAccessReview{Spec: authorizationv1.SubjectAccessReviewSpec{
		User:   user,
		Groups: groups,
		ResourceAttributes: &authorizationv1.ResourceAttributes{
			Group:     gr.Group,
			Resource:  gr.Resource,
			Verb:      verb,
			Namespace: obj.Namespace,
			Name:      obj.Name,
		},
	}}
	if err := a.client.Create(ctx, sar); err != nil {
		return false, fmt.Errorf("creating SubjectAccessReview: %w", err)
	}

	a.mu.Lock()
	if len(a.cache) >= maxCacheEntries {
		clear(a.cache)
	}
	a.cache[key] = cachedAnswer{allowed: sar.Status.Allowed, expires: a.now().Add(a.ttl)}
	a.mu.Unlock()
	return sar.Status.Allowed, nil
}
