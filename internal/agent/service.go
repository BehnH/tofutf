package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"

	"github.com/gorilla/mux"
	"github.com/tofutf/tofutf/internal"
	tofutfhttp "github.com/tofutf/tofutf/internal/http"
	"github.com/tofutf/tofutf/internal/http/html"
	"github.com/tofutf/tofutf/internal/organization"
	"github.com/tofutf/tofutf/internal/pubsub"
	"github.com/tofutf/tofutf/internal/rbac"
	tofutfrun "github.com/tofutf/tofutf/internal/run"
	"github.com/tofutf/tofutf/internal/sql"
	"github.com/tofutf/tofutf/internal/sql/pggen"
	"github.com/tofutf/tofutf/internal/tfeapi"
	"github.com/tofutf/tofutf/internal/tokens"
	"github.com/tofutf/tofutf/internal/workspace"
)

type (
	Service interface {
		AddHandlers(r *mux.Router)
		NewAllocator(logger *slog.Logger) *allocator
		NewManager() *manager
		CreateAgentPool(ctx context.Context, opts CreateAgentPoolOptions) (*Pool, error)
		GetAgentPool(ctx context.Context, poolID string) (*Pool, error)
		WatchAgentPools(ctx context.Context) (<-chan pubsub.Event[*Pool], func())
		WatchAgents(ctx context.Context) (<-chan pubsub.Event[*Agent], func())
		WatchJobs(ctx context.Context) (<-chan pubsub.Event[*Job], func())
		CreateAgentToken(ctx context.Context, poolID string, opts CreateAgentTokenOptions) (*agentToken, []byte, error)
		GetAgentToken(ctx context.Context, tokenID string) (*agentToken, error)
		ListAgentTokens(ctx context.Context, poolID string) ([]*agentToken, error)
		DeleteAgentToken(ctx context.Context, tokenID string) (*agentToken, error)

		registerAgent(ctx context.Context, opts registerAgentOptions) (*Agent, error)
		getAgentJobs(ctx context.Context, agentID string) ([]*Job, error)
		updateAgentStatus(ctx context.Context, agentID string, status AgentStatus) error

		startJob(ctx context.Context, spec JobSpec) ([]byte, error)
		finishJob(ctx context.Context, spec JobSpec, opts finishJobOptions) error
	}

	service struct {
		logger *slog.Logger

		organization internal.Authorizer

		tfeapi      *tfe
		api         *api
		web         *webHandlers
		poolBroker  pubsub.SubscriptionService[*Pool]
		agentBroker pubsub.SubscriptionService[*Agent]
		jobBroker   pubsub.SubscriptionService[*Job]
		phases      phaseClient

		db *db
		*registrar
		*tokenFactory
	}

	ServiceOptions struct {
		Logger *slog.Logger
		*sql.Pool
		*sql.Listener
		html.Renderer
		*tfeapi.Responder

		RunService       *tofutfrun.Service
		WorkspaceService *workspace.Service
		TokensService    *tokens.Service
	}

	phaseClient interface {
		StartPhase(ctx context.Context, runID string, phase internal.PhaseType, _ tofutfrun.PhaseStartOptions) (*tofutfrun.Run, error)
		FinishPhase(ctx context.Context, runID string, phase internal.PhaseType, opts tofutfrun.PhaseFinishOptions) (*tofutfrun.Run, error)
		Cancel(ctx context.Context, runID string) error
	}
)

// NewService constructs, and returns a new Service.
func NewService(opts ServiceOptions) Service {
	svc := &service{
		logger:       opts.Logger,
		db:           &db{Pool: opts.Pool},
		organization: &organization.Authorizer{Logger: opts.Logger},
		tokenFactory: &tokenFactory{
			tokens: opts.TokensService,
		},
		phases: opts.RunService,
	}
	svc.tfeapi = &tfe{
		service:   svc,
		Responder: opts.Responder,
	}
	svc.api = &api{
		service:   svc,
		Responder: opts.Responder,
	}
	svc.web = &webHandlers{
		Renderer:   opts.Renderer,
		logger:     opts.Logger,
		svc:        svc,
		workspaces: opts.WorkspaceService,
	}
	svc.registrar = &registrar{
		service: svc,
	}
	svc.poolBroker = pubsub.NewBroker(
		opts.Logger,
		opts.Listener,
		"agent_pools",
		func(ctx context.Context, id string, action sql.Action) (*Pool, error) {
			if action == sql.DeleteAction {
				return &Pool{ID: id}, nil
			}
			return svc.db.getPool(ctx, id)
		},
	)
	svc.agentBroker = pubsub.NewBroker(
		opts.Logger,
		opts.Listener,
		"agents",
		func(ctx context.Context, id string, action sql.Action) (*Agent, error) {
			if action == sql.DeleteAction {
				return &Agent{ID: id}, nil
			}
			return svc.db.getAgent(ctx, id)
		},
	)
	svc.jobBroker = pubsub.NewBroker(
		opts.Logger,
		opts.Listener,
		"jobs",
		func(ctx context.Context, specStr string, action sql.Action) (*Job, error) {
			spec, err := jobSpecFromString(specStr)
			if err != nil {
				return nil, err
			}
			if action == sql.DeleteAction {
				return &Job{Spec: spec}, nil
			}
			return svc.db.getJob(ctx, spec)
		},
	)
	// create jobs when a plan or apply is enqueued
	opts.RunService.AfterEnqueuePlan(svc.createJob)
	opts.RunService.AfterEnqueueApply(svc.createJob)
	// cancel job when a run is canceled
	opts.RunService.AfterCancelRun(svc.cancelJob)
	// cancel job when a run is forceably canceled
	opts.RunService.AfterForceCancelRun(svc.cancelJob)
	// check whether a workspace is being created or updated and configured to
	// use an agent pool, and if so, check that it is allowed to use the pool.
	opts.WorkspaceService.BeforeCreateWorkspace(svc.checkWorkspacePoolAccess)
	opts.WorkspaceService.BeforeUpdateWorkspace(svc.checkWorkspacePoolAccess)
	// Register with auth middleware the agent token kind and a means of
	// retrieving the appropriate agent corresponding to the agent token ID
	opts.TokensService.RegisterKind(AgentTokenKind, func(ctx context.Context, tokenID string) (internal.Subject, error) {
		pool, err := svc.db.getPoolByTokenID(ctx, tokenID)
		if err != nil {
			return nil, err
		}
		unregistered := &unregisteredPoolAgent{
			pool:         pool,
			agentTokenID: tokenID,
		}
		// if the agent has registered then it should be sending its ID in an
		// http header
		headers, err := tofutfhttp.HeadersFromContext(ctx)
		if err != nil {
			return nil, err
		}
		if agentID := headers.Get(agentIDHeader); agentID != "" {
			agent, err := svc.getAgent(ctx, agentID)
			if err != nil {
				return nil, fmt.Errorf("retrieving agent corresponding to ID found in http header: %w", err)
			}
			return &poolAgent{
				agent:                 agent,
				unregisteredPoolAgent: unregistered,
			}, nil
		}
		return unregistered, nil
	})
	// Register with auth middleware the job token and a means of
	// retrieving Job corresponding to token.
	opts.TokensService.RegisterKind(JobTokenKind, func(ctx context.Context, jobspecString string) (internal.Subject, error) {
		spec, err := jobSpecFromString(jobspecString)
		if err != nil {
			return nil, err
		}
		return svc.getJob(ctx, spec)
	})

	return NewServiceWithTracing(svc, "agent.Service")
}

func (s *service) AddHandlers(r *mux.Router) {
	s.tfeapi.addHandlers(r)
	s.api.addHandlers(r)
	s.web.addHandlers(r)
}

func (s *service) NewAllocator(logger *slog.Logger) *allocator {
	return &allocator{
		logger: logger,
		client: s,
	}
}

func (s *service) NewManager() *manager { return newManager(s) }

func (s *service) CreateAgentPool(ctx context.Context, opts CreateAgentPoolOptions) (*Pool, error) {
	subject, err := s.organization.CanAccess(ctx, rbac.CreateAgentPoolAction, opts.Organization)
	if err != nil {
		return nil, err
	}
	pool, err := newPool(opts)
	if err != nil {
		s.logger.Error("creating agent pool", "subject", subject, "err", err)
		return nil, err
	}
	if err := s.db.createPool(ctx, pool); err != nil {
		return nil, err
	}
	s.logger.Info("created agent pool", "subject", subject, "pool", pool)
	return pool, nil
}

func (s *service) updateAgentPool(ctx context.Context, poolID string, opts updatePoolOptions) (*Pool, error) {
	var (
		subject       internal.Subject
		before, after Pool
	)
	err := s.db.Lock(ctx, "agent_pools, agent_pool_allowed_workspaces", func(ctx context.Context, q pggen.Querier) (err error) {
		pool, err := s.db.getPool(ctx, poolID)
		if err != nil {
			return err
		}
		subject, err = s.organization.CanAccess(ctx, rbac.UpdateAgentPoolAction, pool.Organization)
		if err != nil {
			return err
		}
		before = *pool
		after = *pool
		if err := after.update(opts); err != nil {
			return err
		}
		if err := s.db.updatePool(ctx, &after); err != nil {
			return err
		}
		// Add/remove allowed workspaces
		add := internal.DiffStrings(after.AllowedWorkspaces, before.AllowedWorkspaces)
		remove := internal.DiffStrings(before.AllowedWorkspaces, after.AllowedWorkspaces)
		for _, workspaceID := range add {
			if err := s.db.addAgentPoolAllowedWorkspace(ctx, poolID, workspaceID); err != nil {
				return err
			}
		}
		for _, workspaceID := range remove {
			if err := s.db.deleteAgentPoolAllowedWorkspace(ctx, poolID, workspaceID); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		s.logger.Error("updating agent pool", "agent_pool_id", poolID, "subject", subject, "err", err)
		return nil, err
	}
	s.logger.Info("updated agent pool", "subject", subject, "before", &before, "after", &after)
	return &after, nil
}

func (s *service) GetAgentPool(ctx context.Context, poolID string) (*Pool, error) {
	pool, err := s.db.getPool(ctx, poolID)
	if err != nil {
		s.logger.Error("retrieving agent pool", "agent_pool_id", poolID, "err", err)
		return nil, err
	}

	subject, err := s.organization.CanAccess(ctx, rbac.GetAgentPoolAction, pool.Organization)
	if err != nil {
		return nil, err
	}

	s.logger.Debug("retrieved agent pool", "subject", subject, "organization", pool.Organization)
	return pool, nil
}

func (s *service) listAllAgentPools(ctx context.Context) ([]*Pool, error) {
	subject, err := internal.SubjectFromContext(ctx)
	if err != nil {
		return nil, err
	}
	pools, err := s.db.listPools(ctx)
	if err != nil {
		s.logger.Error("listing all agent pools", "subject", subject, "err", err)
		return nil, err
	}
	s.logger.Debug("listed all agent pools", "subject", subject, "count", len(pools))
	return pools, nil
}

func (s *service) listAgentPoolsByOrganization(ctx context.Context, organization string, opts listPoolOptions) ([]*Pool, error) {
	subject, err := s.organization.CanAccess(ctx, rbac.ListAgentPoolsAction, organization)
	if err != nil {
		return nil, err
	}
	pools, err := s.db.listPoolsByOrganization(ctx, organization, opts)
	if err != nil {
		s.logger.Error("listing agent pools", "subject", subject, "err", err)
		return nil, err
	}
	s.logger.Debug("listed agent pools", "subject", subject, "count", len(pools))
	return pools, nil
}

func (s *service) deleteAgentPool(ctx context.Context, poolID string) (*Pool, error) {
	pool, subject, err := func() (*Pool, internal.Subject, error) {
		// retrieve pool in order to get organization for authorization
		pool, err := s.db.getPool(ctx, poolID)
		if err != nil {
			return nil, nil, err
		}
		subject, err := s.organization.CanAccess(ctx, rbac.DeleteAgentPoolAction, pool.Organization)
		if err != nil {
			return nil, nil, err
		}
		// only permit pool to be deleted if it is not referenced by any
		// workspaces (it would raise a foreign key error anyway but friendlier
		// to return an informative error message).
		if len(pool.AssignedWorkspaces) > 0 {
			return nil, nil, ErrCannotDeletePoolReferencedByWorkspaces
		}
		if err := s.db.deleteAgentPool(ctx, pool.ID); err != nil {
			return nil, subject, err
		}
		return pool, subject, nil
	}()
	if err != nil {
		s.logger.Error("deleting agent pool", "agent_pool_id", poolID, "subject", subject, "err", err)
		return nil, err
	}
	s.logger.Debug("deleted agent pool", "pool", pool, "subject", subject)
	return pool, nil
}

// checkWorkspacePoolAccess checks if a workspace has been granted access to a pool. If the
// pool is organization-scoped then the workspace automatically has access;
// otherwise access must already have been granted explicity.
func (s *service) checkWorkspacePoolAccess(ctx context.Context, ws *workspace.Workspace) error {
	if ws.AgentPoolID == nil {
		// workspace is not using any pool
		return nil
	}
	pool, err := s.GetAgentPool(ctx, *ws.AgentPoolID)
	if err != nil {
		return err
	}
	if pool.OrganizationScoped {
		return nil
	} else if slices.Contains(pool.AllowedWorkspaces, ws.ID) {
		// is explicitly granted
		return nil
	}
	return ErrWorkspaceNotAllowedToUsePool
}

func (s *service) WatchAgentPools(ctx context.Context) (<-chan pubsub.Event[*Pool], func()) {
	return s.poolBroker.Subscribe(ctx)
}

func (s *service) WatchAgents(ctx context.Context) (<-chan pubsub.Event[*Agent], func()) {
	return s.agentBroker.Subscribe(ctx)
}

func (s *service) WatchJobs(ctx context.Context) (<-chan pubsub.Event[*Job], func()) {
	return s.jobBroker.Subscribe(ctx)
}

func (s *service) registerAgent(ctx context.Context, opts registerAgentOptions) (*Agent, error) {
	agent, err := func() (*Agent, error) {
		// subject must be an unregistered agent
		subject, err := internal.SubjectFromContext(ctx)
		if err != nil {
			return nil, err
		}
		switch agent := subject.(type) {
		case *unregisteredServerAgent:
		case *unregisteredPoolAgent:
			// extract pool ID and use for registration.
			opts.AgentPoolID = &agent.pool.ID
		default:
			return nil, ErrUnauthorizedAgentRegistration
		}

		agent, err := s.register(ctx, opts)
		if err != nil {
			return nil, err
		}
		err = s.db.Tx(ctx, func(ctx context.Context, q pggen.Querier) error {
			if err := s.db.createAgent(ctx, agent); err != nil {
				return err
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
		return agent, nil
	}()
	if err != nil {
		s.logger.Error("registering agent", "err", err)
		return nil, err
	}
	s.logger.Info("registered agent", "agent", agent)
	return agent, nil
}

func (s *service) getAgent(ctx context.Context, agentID string) (*Agent, error) {
	return s.db.getAgent(ctx, agentID)
}

func (s *service) updateAgentStatus(ctx context.Context, agentID string, to AgentStatus) error {
	// only these subjects may call this endpoint:
	// (a) the manager, or
	// (b) an agent with an ID matching agentID
	subject, err := internal.SubjectFromContext(ctx)
	if err != nil {
		return err
	}
	var isAgent bool
	switch s := subject.(type) {
	case *manager:
		// ok
	case *serverAgent, *poolAgent:
		if s.String() != agentID {
			return internal.ErrAccessNotPermitted
		}
		isAgent = true
	default:
		return internal.ErrAccessNotPermitted
	}

	// keep a record of what the status was before the update for logging
	// purposes
	var from AgentStatus
	err = s.db.updateAgent(ctx, agentID, func(agent *Agent) error {
		from = agent.Status
		return agent.setStatus(to, isAgent)
	})
	if err != nil {
		s.logger.Error("updating agent status", "agent_id", agentID, "status", to, "subject", subject, "err", err)
		return err
	}
	if isAgent && from == to {
		// if no change in status then log it as a ping
		s.logger.Debug("received agent ping", "agent_id", agentID)
	} else {
		s.logger.Debug("updated agent status", "agent_id", agentID, "from", from, "to", to, "subject", subject)
	}
	return nil
}

func (s *service) listAgents(ctx context.Context) ([]*Agent, error) {
	return s.db.listAgents(ctx)
}

func (s *service) listServerAgents(ctx context.Context) ([]*Agent, error) {
	return s.db.listServerAgents(ctx)
}

func (s *service) listAgentsByOrganization(ctx context.Context, organization string) ([]*Agent, error) {
	_, err := s.organization.CanAccess(ctx, rbac.ListAgentsAction, organization)
	if err != nil {
		return nil, err
	}
	return s.db.listAgentsByOrganization(ctx, organization)
}

func (s *service) listAgentsByPool(ctx context.Context, poolID string) ([]*Agent, error) {
	return s.db.listAgentsByPool(ctx, poolID)
}

func (s *service) deleteAgent(ctx context.Context, agentID string) error {
	if err := s.db.deleteAgent(ctx, agentID); err != nil {
		s.logger.Error("deleting agent", "agent_id", agentID, "err", err)
		return err
	}
	s.logger.Debug("deleted agent", "agent_id", agentID)
	return nil
}

func (s *service) createJob(ctx context.Context, run *tofutfrun.Run) error {
	job := newJob(run)
	if err := s.db.createJob(ctx, job); err != nil {
		return err
	}
	return nil
}

// cancelJob is called when a user cancels a run - cancelJob determines whether
// the corresponding job is signaled and what type of signal, and/or whether the
// job should be canceled.
func (s *service) cancelJob(ctx context.Context, run *tofutfrun.Run) error {
	var (
		spec   = JobSpec{RunID: run.ID, Phase: run.Phase()}
		signal *bool
	)
	job, err := s.db.updateJob(ctx, spec, func(job *Job) (err error) {
		signal, err = job.cancel(run)
		return err
	})
	if err != nil {
		if errors.Is(err, internal.ErrResourceNotFound) {
			// ignore when no job has yet been created for the run.
			return nil
		}
		s.logger.Error("canceling job", "spec", spec, "err", err)
		return err
	}
	if signal != nil {
		s.logger.Debug("sending cancelation signal to job", "force-cancel", *signal, "job", job)
	} else {
		s.logger.Info("canceled job", "job", job)
	}

	return nil
}

// getAgentJobs returns jobs that either:
// (a) have JobAllocated status
// (b) have JobRunning status and a non-nil signal
//
// getAgentJobs is intended to be called by an agent in order to retrieve jobs to
// execute and jobs to cancel.
func (s *service) getAgentJobs(ctx context.Context, agentID string) ([]*Job, error) {
	// only these subjects may call this endpoint:
	// (a) an agent with an ID matching agentID
	subject, err := internal.SubjectFromContext(ctx)
	if err != nil {
		return nil, err
	}
	switch s := subject.(type) {
	case *serverAgent, *poolAgent:
		if s.String() != agentID {
			return nil, internal.ErrAccessNotPermitted
		}
	default:
		return nil, internal.ErrAccessNotPermitted
	}

	sub, unsub := s.WatchJobs(ctx)
	defer unsub()
	jobs, err := s.db.getAllocatedAndSignaledJobs(ctx, agentID)
	if err != nil {
		return nil, err
	}
	if len(jobs) > 0 {
		// return existing jobs
		return jobs, nil
	}
	// wait for a job matching criteria to arrive:
	for event := range sub {
		job := event.Payload
		if job.AgentID == nil || *job.AgentID != agentID {
			continue
		}
		switch job.Status {
		case JobAllocated:
			return []*Job{job}, nil
		case JobRunning:
			if job.Signaled != nil {
				return []*Job{job}, nil
			}
		}
	}
	return nil, nil
}

func (s *service) getJob(ctx context.Context, spec JobSpec) (*Job, error) {
	return s.db.getJob(ctx, spec)
}

func (s *service) listJobs(ctx context.Context) ([]*Job, error) {
	return s.db.listJobs(ctx)
}

func (s *service) allocateJob(ctx context.Context, spec JobSpec, agentID string) (*Job, error) {
	allocated, err := s.db.updateJob(ctx, spec, func(job *Job) error {
		return job.allocate(agentID)
	})
	if err != nil {
		s.logger.Error("allocating job", "spec", spec, "agent_id", agentID, "err", err)
		return nil, err
	}

	s.logger.Info("allocated job", "job", allocated, "agent_id", agentID)
	return allocated, nil
}

func (s *service) reallocateJob(ctx context.Context, spec JobSpec, agentID string) (*Job, error) {
	var (
		from        string // ID of agent that job *was* allocated to
		reallocated *Job
	)
	reallocated, err := s.db.updateJob(ctx, spec, func(job *Job) error {
		from = *job.AgentID
		return job.reallocate(agentID)
	})
	if err != nil {
		s.logger.Error("re-allocating job", "spec", spec, "from", from, "to", agentID, "err", err)
		return nil, err
	}
	s.logger.Info("re-allocated job", "spec", spec, "from", from, "to", agentID)
	return reallocated, nil
}

// startJob starts a job and returns a job token with permissions to
// carry out the job. Only an agent that has been allocated the job can
// call this method.
func (s *service) startJob(ctx context.Context, spec JobSpec) ([]byte, error) {
	subject, err := registeredAgentFromContext(ctx)
	if err != nil {
		return nil, internal.ErrAccessNotPermitted
	}

	var token []byte
	_, err = s.db.updateJob(ctx, spec, func(job *Job) error {
		if job.AgentID == nil || *job.AgentID != subject.String() {
			return internal.ErrAccessNotPermitted
		}
		if err := job.startJob(); err != nil {
			return err
		}
		// start corresponding run phase too
		if _, err = s.phases.StartPhase(ctx, spec.RunID, spec.Phase, tofutfrun.PhaseStartOptions{}); err != nil {
			return err
		}
		token, err = s.tokenFactory.createJobToken(spec)
		if err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		s.logger.Error("starting job", "spec", spec, "agent", subject, "err", err)
		return nil, err
	}
	s.logger.Debug("started job", "spec", spec, "agent", subject)
	return token, nil
}

type finishJobOptions struct {
	Status JobStatus `json:"status"`
	Error  string    `json:"error,omitempty"`
}

// finishJob finishes a job. Only the job itself may call this endpoint.
func (s *service) finishJob(ctx context.Context, spec JobSpec, opts finishJobOptions) error {
	{
		subject, err := internal.SubjectFromContext(ctx)
		if err != nil {
			return internal.ErrAccessNotPermitted
		}
		_, ok := subject.(*Job)
		if !ok {
			return internal.ErrAccessNotPermitted
		}
	}
	job, err := s.db.updateJob(ctx, spec, func(job *Job) error {
		// update corresponding run phase too
		var err error
		switch opts.Status {
		case JobFinished, JobErrored:
			_, err = s.phases.FinishPhase(ctx, spec.RunID, spec.Phase, tofutfrun.PhaseFinishOptions{
				Errored: opts.Status == JobErrored,
			})
		case JobCanceled:
			err = s.phases.Cancel(ctx, spec.RunID)
		}
		if err != nil {
			return err
		}
		return job.finishJob(opts.Status)
	})
	if err != nil {
		s.logger.Error("finishing job", "spec", spec, "err", err)
		return err
	}
	if opts.Error != "" {
		s.logger.Debug("finished job with error", "job", job, "status", opts.Status, "job_error", opts.Error)
	} else {
		s.logger.Debug("finished job", "job", job, "status", opts.Status)
	}
	return nil
}

// agent tokens

func (s *service) CreateAgentToken(ctx context.Context, poolID string, opts CreateAgentTokenOptions) (*agentToken, []byte, error) {
	at, token, subject, err := func() (*agentToken, []byte, internal.Subject, error) {
		pool, err := s.db.getPool(ctx, poolID)
		if err != nil {
			return nil, nil, nil, err
		}

		subject, err := s.organization.CanAccess(ctx, rbac.CreateAgentTokenAction, pool.Organization)
		if err != nil {
			return nil, nil, nil, err
		}

		at, token, err := s.NewAgentToken(poolID, opts)
		if err != nil {
			return nil, nil, nil, err
		}

		if err := s.db.createAgentToken(ctx, at); err != nil {
			s.logger.Error("creating agent token", "organization", poolID, "id", at.ID, "subject", subject, "err", err)
			return nil, nil, nil, err
		}

		return at, token, subject, nil
	}()
	if err != nil {
		s.logger.Error("creating agent token", "agent_pool_id", poolID, "subject", subject, "err", err)
		return nil, nil, err
	}
	s.logger.Info("created agent token", "token", at, "subject", subject)
	return at, token, nil
}

func (s *service) GetAgentToken(ctx context.Context, tokenID string) (*agentToken, error) {
	at, subject, err := func() (*agentToken, internal.Subject, error) {
		at, err := s.db.getAgentTokenByID(ctx, tokenID)
		if err != nil {
			return nil, nil, err
		}
		pool, err := s.db.getPool(ctx, at.AgentPoolID)
		if err != nil {
			return nil, nil, err
		}
		subject, err := s.organization.CanAccess(ctx, rbac.GetAgentTokenAction, pool.Organization)
		if err != nil {
			return nil, nil, err
		}
		return at, subject, nil
	}()
	if err != nil {
		s.logger.Error("retrieving agent token", "id", tokenID, "err", err)
		return nil, err
	}
	s.logger.Debug("retrieved agent token", "token", at, "subject", subject)
	return at, nil
}

func (s *service) ListAgentTokens(ctx context.Context, poolID string) ([]*agentToken, error) {
	pool, err := s.db.getPool(ctx, poolID)
	if err != nil {
		return nil, err
	}
	subject, err := s.organization.CanAccess(ctx, rbac.ListAgentTokensAction, pool.Organization)
	if err != nil {
		return nil, err
	}

	tokens, err := s.db.listAgentTokens(ctx, poolID)
	if err != nil {
		s.logger.Error("listing agent tokens", "organization", poolID, "subject", subject, "err", err)
		return nil, err
	}
	s.logger.Info("listed agent tokens", "organization", poolID, "subject", subject)
	return tokens, nil
}

func (s *service) DeleteAgentToken(ctx context.Context, tokenID string) (*agentToken, error) {
	at, subject, err := func() (*agentToken, internal.Subject, error) {
		// retrieve agent token and pool in order to get organization for authorization
		at, err := s.db.getAgentTokenByID(ctx, tokenID)
		if err != nil {
			return nil, nil, err
		}
		pool, err := s.db.getPool(ctx, at.AgentPoolID)
		if err != nil {
			return nil, nil, err
		}
		subject, err := s.organization.CanAccess(ctx, rbac.DeleteAgentTokenAction, pool.Organization)
		if err != nil {
			return nil, nil, err
		}
		if err := s.db.deleteAgentToken(ctx, tokenID); err != nil {
			return nil, subject, err
		}
		return at, subject, nil
	}()
	if err != nil {
		s.logger.Error("deleting agent token", "id", tokenID, "err", err)
		return nil, err
	}

	s.logger.Info("deleted agent token", "token", at, "subject", subject)
	return at, nil
}
