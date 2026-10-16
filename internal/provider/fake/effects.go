package fake

import (
	"context"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/compliance-framework/factory/internal/apis/v1alpha1"
	"github.com/compliance-framework/factory/internal/provider"
)

// MaxCredentialTTL caps a minted token's lifetime, as GitHub does.
const MaxCredentialTTL = time.Hour

// ProjectCall is one recorded Project call.
type ProjectCall struct {
	Ref        v1alpha1.ObjectRef
	Projection provider.Projection
	// Changed is false when the object already carried this projection and its labels.
	Changed bool
	Err     error
}

// ApplyCall is one recorded Apply call.
type ApplyCall struct {
	Request provider.EffectRequest
	Result  provider.EffectResult
	Err     error
}

// MintCall is one recorded MintGitCredential call.
type MintCall struct {
	Request    provider.GitCredentialRequest
	Credential provider.Credential
	Err        error
}

// Projections returns every Project call, in order.
func (c *Conn) Projections() []ProjectCall {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]ProjectCall, len(c.projects))
	for i, p := range c.projects {
		p.Projection = cloneProjection(p.Projection)
		out[i] = p
	}
	return out
}

// Applies returns every Apply call, in order: replays and failures included.
func (c *Conn) Applies() []ApplyCall {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]ApplyCall, len(c.applies))
	for i, a := range c.applies {
		a.Request, a.Result = cloneEffectRequest(a.Request), cloneEffectResult(a.Result)
		out[i] = a
	}
	return out
}

// Effects returns the effects that were applied: the successful Apply calls that were not
// replays, in order.
func (c *Conn) Effects() []ApplyCall {
	var out []ApplyCall
	for _, a := range c.Applies() {
		if a.Err == nil && !a.Result.Replayed {
			out = append(out, a)
		}
	}
	return out
}

// Mints returns every MintGitCredential call, in order.
func (c *Conn) Mints() []MintCall {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]MintCall, len(c.mints))
	for i, m := range c.mints {
		m.Request.ReadRepos, m.Request.Permissions = slices.Clone(m.Request.ReadRepos), maps.Clone(m.Request.Permissions)
		m.Credential.ReadRepos, m.Credential.Permissions = slices.Clone(m.Credential.ReadRepos), maps.Clone(m.Credential.Permissions)
		out[i] = m
	}
	return out
}

// Project implements provider.Conn: it stores the projection on the object (replacing the
// work item's previous one; Observe returns it in Projections) and adds the projection's
// labels the object lacks. Unknown objects are ErrNotFound.
func (c *Conn) Project(ctx context.Context, ref v1alpha1.ObjectRef, p provider.Projection) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	p = cloneProjection(p)
	call := ProjectCall{Ref: ref, Projection: p}
	call.Changed, call.Err = c.projectLocked(ref, p)
	c.projects = append(c.projects, call)
	return call.Err
}

func (c *Conn) projectLocked(ref v1alpha1.ObjectRef, p provider.Projection) (bool, error) {
	if err := c.faultLocked(MethodProject); err != nil {
		return false, err
	}
	if p.WorkItemID == "" {
		return false, fmt.Errorf("fake: project: projection has no work item ID: %w", provider.ErrInvalid)
	}
	o, err := c.lookupLocked(ref)
	if err != nil {
		return false, fmt.Errorf("fake: project: %w", err)
	}
	changed := false
	for _, l := range p.Labels {
		if !slices.Contains(o.Labels, l) {
			o.Labels = append(o.Labels, l)
			changed = true
		}
	}
	i := slices.IndexFunc(o.Projections, func(q provider.Projection) bool { return q.WorkItemID == p.WorkItemID })
	switch {
	case i < 0:
		o.Projections = append(o.Projections, cloneProjection(p))
		slices.SortFunc(o.Projections, func(a, b provider.Projection) int { return strings.Compare(a.WorkItemID, b.WorkItemID) })
		changed = true
	case !reflect.DeepEqual(o.Projections[i], p):
		o.Projections[i] = cloneProjection(p)
		changed = true
	}
	if changed {
		o.UpdatedAt = c.opts.Clock.Now()
	}
	return changed, nil
}

// Apply implements provider.Conn. The first Apply of an idempotency key changes the objects
// as the op says and stores the result; a later Apply of the key (whatever its op) changes
// nothing and returns that result with Replayed set. A failed Apply stores nothing, so a retry
// runs again. Every call is recorded.
//
// Modelled ops: comment ops count on the issue; label.add/remove (args.labels or args.label);
// close; link-duplicate (label "duplicate" and close); create-issue and pr.open (Target.Ref is
// the repo; args.labels, args.base, args.head, args.draft) create the next number in the
// repo; sub-issue.add (args.issue: a ref); pr.retarget (args.base); pr.merge; review.approve
// (approves the head for Identity); thread.resolve. Other API ops only need the target to
// exist. Git-side ops are ErrUnsupported (the agent executes them).
func (c *Conn) Apply(ctx context.Context, e provider.EffectRequest) (provider.EffectResult, error) {
	if err := ctx.Err(); err != nil {
		return provider.EffectResult{}, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	e = cloneEffectRequest(e)
	res, err := c.applyLocked(e)
	c.applies = append(c.applies, ApplyCall{Request: e, Result: cloneEffectResult(res), Err: err})
	return res, err
}

func (c *Conn) applyLocked(e provider.EffectRequest) (provider.EffectResult, error) {
	if err := c.faultLocked(MethodApply); err != nil {
		return provider.EffectResult{}, err
	}
	side, ok := e.Op.Side()
	switch {
	case !ok:
		return provider.EffectResult{}, fmt.Errorf("fake: apply: unknown op %q: %w", e.Op, provider.ErrInvalid)
	case side != v1alpha1.SideAPI:
		return provider.EffectResult{}, fmt.Errorf("fake: apply %s: git-side op: %w", e.Op, provider.ErrUnsupported)
	case e.IdempotencyKey == "":
		return provider.EffectResult{}, fmt.Errorf("fake: apply %s: no idempotency key: %w", e.Op, provider.ErrInvalid)
	}
	if res, ok := c.applied[e.IdempotencyKey]; ok {
		res = cloneEffectResult(res)
		res.Replayed = true
		return res, nil
	}
	var (
		res provider.EffectResult
		err error
	)
	switch e.Op {
	case v1alpha1.OpCreateIssue, v1alpha1.OpPROpen:
		res, err = c.createLocked(e)
	default:
		res, err = c.mutateLocked(e)
	}
	if err != nil {
		return provider.EffectResult{}, fmt.Errorf("fake: apply %s: %w", e.Op, err)
	}
	c.applied[e.IdempotencyKey] = cloneEffectResult(res)
	return res, nil
}

// createLocked creates an issue or a PR in the repo e.Target.Ref.
func (c *Conn) createLocked(e provider.EffectRequest) (provider.EffectResult, error) {
	repo := e.Target.Ref
	if repo == "" || strings.Contains(repo, "#") {
		return provider.EffectResult{}, fmt.Errorf("target %q is not a repo: %w", repo, provider.ErrInvalid)
	}
	if e.Target.Provider != "" && e.Target.Provider != c.p.Metadata.Name {
		return provider.EffectResult{}, fmt.Errorf("target is on provider %q: %w", e.Target.Provider, provider.ErrInvalid)
	}
	next := 1
	for _, o := range c.objects {
		r, num, ok := strings.Cut(o.Ref.Ref, "#")
		if n, err := strconv.Atoi(num); ok && err == nil && r == repo && n >= next {
			next = n + 1
		}
	}
	o := &provider.Observation{
		Ref:       v1alpha1.ObjectRef{Provider: c.p.Metadata.Name, Kind: provider.KindIssue, Ref: fmt.Sprintf("%s#%d", repo, next)},
		State:     provider.StateOpen,
		Labels:    sortedLabels(stringsArg(e.Args, "labels")),
		Author:    c.identityActorLocked(e.Identity),
		UpdatedAt: c.opts.Clock.Now(),
	}
	if e.Op == v1alpha1.OpPROpen {
		o.Ref.Kind = provider.KindPR
		draft, _ := e.Args["draft"].(bool)
		o.PR = &provider.PRObservation{Draft: draft, Head: stringArg(e.Args, "head"), Base: stringArg(e.Args, "base")}
	} else {
		o.Issue = &provider.IssueObservation{}
	}
	c.objects[o.Ref.Ref] = o
	ref := o.Ref
	return provider.EffectResult{URL: c.objectURL(ref), Object: &ref}, nil
}

// identityActorLocked is the actor of a factory identity, from the configured App logins.
func (c *Conn) identityActorLocked(identity string) *v1alpha1.Actor {
	for login, id := range c.opts.Identities {
		if id == identity {
			return &v1alpha1.Actor{ID: c.p.Metadata.Name + ":" + login, IsBot: true, IsFactory: true, Identity: id}
		}
	}
	return nil
}

// mutateLocked applies an op to an existing object.
func (c *Conn) mutateLocked(e provider.EffectRequest) (provider.EffectResult, error) {
	o, err := c.lookupLocked(e.Target)
	if err != nil {
		return provider.EffectResult{}, err
	}
	ref := o.Ref
	res := provider.EffectResult{URL: c.objectURL(ref), Object: &ref}
	pr := func() (*provider.PRObservation, error) {
		if o.PR == nil {
			return nil, fmt.Errorf("%s is not a PR: %w", ref.Ref, provider.ErrInvalid)
		}
		return o.PR, nil
	}
	switch e.Op {
	case v1alpha1.OpComment, v1alpha1.OpPRComment, v1alpha1.OpReviewComment, v1alpha1.OpReviewRequestChanges,
		v1alpha1.OpThreadReply:
		c.seq++
		res.URL += "#comment-" + strconv.Itoa(c.seq)
		if o.Issue != nil {
			o.Issue.Comments++
		}
	case v1alpha1.OpLabelAdd:
		o.Labels = sortedLabels(append(o.Labels, labelsArg(e.Args)...))
	case v1alpha1.OpLabelRemove:
		drop := labelsArg(e.Args)
		o.Labels = slices.DeleteFunc(o.Labels, func(l string) bool { return slices.Contains(drop, l) })
	case v1alpha1.OpClose:
		o.State = provider.StateClosed
	case v1alpha1.OpLinkDuplicate:
		o.Labels = sortedLabels(append(o.Labels, "duplicate"))
		o.State = provider.StateClosed
	case v1alpha1.OpSubIssueAdd:
		sub := stringArg(e.Args, "issue")
		if o.Issue == nil || sub == "" {
			return provider.EffectResult{}, fmt.Errorf("sub-issue.add needs an issue target and args.issue: %w", provider.ErrInvalid)
		}
		subRef := v1alpha1.ObjectRef{Provider: c.p.Metadata.Name, Kind: provider.KindIssue, Ref: sub}
		if !slices.Contains(o.Issue.SubIssues, subRef) {
			o.Issue.SubIssues = append(o.Issue.SubIssues, subRef)
		}
	case v1alpha1.OpPRRetarget:
		p, err := pr()
		if err != nil {
			return provider.EffectResult{}, err
		}
		p.Base = stringArg(e.Args, "base")
	case v1alpha1.OpPRMerge:
		p, err := pr()
		if err != nil {
			return provider.EffectResult{}, err
		}
		if o.State != provider.StateMerged {
			c.seq++
			o.State, p.MergedAt, p.MergeCommit = provider.StateMerged, c.opts.Clock.Now(), fmt.Sprintf("merge-%d", c.seq)
		}
	case v1alpha1.OpReviewApprove:
		p, err := pr()
		if err != nil {
			return provider.EffectResult{}, err
		}
		p.ApprovedBy = append(p.ApprovedBy, provider.Approval{Identity: e.Identity, Commit: p.Head})
	case v1alpha1.OpThreadResolve:
		p, err := pr()
		if err != nil {
			return provider.EffectResult{}, err
		}
		p.UnresolvedThreads = max(0, p.UnresolvedThreads-1)
	}
	o.UpdatedAt = c.opts.Clock.Now()
	return res, nil
}

// MintGitCredential implements provider.Conn: a fake token ("fake-<identity>-<n>") for exactly
// the requested repos and permissions, expiring after TTL (capped at MaxCredentialTTL). A
// request without identity, repo or a positive TTL is ErrInvalid, as is an identity the
// Provider's github.identities doesn't list (when it lists any).
func (c *Conn) MintGitCredential(ctx context.Context, req provider.GitCredentialRequest) (provider.Credential, error) {
	if err := ctx.Err(); err != nil {
		return provider.Credential{}, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	req.ReadRepos = slices.Clone(req.ReadRepos)
	req.Permissions = maps.Clone(req.Permissions)
	cred, err := c.mintLocked(req)
	c.mints = append(c.mints, MintCall{Request: req, Credential: cred, Err: err})
	return cred, err
}

func (c *Conn) mintLocked(req provider.GitCredentialRequest) (provider.Credential, error) {
	if err := c.faultLocked(MethodMintGitCredential); err != nil {
		return provider.Credential{}, err
	}
	switch {
	case req.Identity == "":
		return provider.Credential{}, fmt.Errorf("fake: mint: no identity: %w", provider.ErrInvalid)
	case req.Repo == "":
		return provider.Credential{}, fmt.Errorf("fake: mint: no repo (unscoped token): %w", provider.ErrInvalid)
	case req.TTL <= 0:
		return provider.Credential{}, fmt.Errorf("fake: mint: TTL %s: %w", req.TTL, provider.ErrInvalid)
	}
	if gh := c.p.Spec.GitHub; gh != nil && len(gh.Identities) > 0 {
		if _, ok := gh.Identities[req.Identity]; !ok {
			return provider.Credential{}, fmt.Errorf("fake: mint: unknown identity %q: %w", req.Identity, provider.ErrInvalid)
		}
	}
	c.seq++
	return provider.Credential{
		Provider:    c.p.Metadata.Name,
		Host:        c.opts.Host,
		Username:    "x-access-token",
		Token:       fmt.Sprintf("fake-%s-%d", req.Identity, c.seq),
		ExpiresAt:   c.opts.Clock.Now().Add(min(req.TTL, MaxCredentialTTL)),
		Repo:        req.Repo,
		ReadRepos:   slices.Clone(req.ReadRepos),
		Permissions: maps.Clone(req.Permissions),
	}, nil
}

// stringArg returns args[key] when it is a string.
func stringArg(args map[string]any, key string) string {
	s, _ := args[key].(string)
	return s
}

// stringsArg returns args[key] as strings: a []string, or the strings of a []any.
func stringsArg(args map[string]any, key string) []string {
	switch v := args[key].(type) {
	case []string:
		return slices.Clone(v)
	case []any:
		out := make([]string, 0, len(v))
		for _, e := range v {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// labelsArg is args.labels plus args.label.
func labelsArg(args map[string]any) []string {
	ls := stringsArg(args, "labels")
	if l := stringArg(args, "label"); l != "" {
		ls = append(ls, l)
	}
	return ls
}

// sortedLabels sorts and dedupes labels; nil when there are none.
func sortedLabels(ls []string) []string {
	if len(ls) == 0 {
		return nil
	}
	return sortedUnique(ls)
}
