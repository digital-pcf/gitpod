// Copyright (c) 2020 TypeFox GmbH. All rights reserved.
// Licensed under the Gitpod Enterprise Source Code License,
// See License.enterprise.txt in the project root folder.

package scaler

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/gitpod-io/gitpod/common-go/log"
	"github.com/gitpod-io/gitpod/common-go/util"
	csapi "github.com/gitpod-io/gitpod/content-service/api"
	"github.com/gitpod-io/gitpod/ws-manager/api"
	"github.com/golang/protobuf/ptypes"
	"github.com/google/uuid"
	"golang.org/x/xerrors"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	// DefaultGhostOwner is the UID of the default owner for ghost workspaces
	DefaultGhostOwner = "00000000-0000-0000-0000-000000000000"
	maxGhostStartTime = 15 * time.Minute
)

// WorkspaceManagerPrescaleDriverConfig configures a ws-manager based prescale driver
type WorkspaceManagerPrescaleDriverConfig struct {
	WorkspaceManagerAddr string `json:"wsmanAddr"`
	GhostOwner           string `json:"ghostOwner"`
	WorkspaceImage       string `json:"workspaceImage"`
	IDEImage             string `json:"ideImage"`

	MaxGhostWorkspaces int           `json:"maxGhostWorkspaces"`
	ReactionDelay      util.Duration `json:"reactionDelay"`

	Renewal struct {
		Interval   util.Duration `json:"interval"`
		Percentage int           `json:"percentage"`
	} `json:"renewal"`
}

// NewWorkspaceManagerPrescaleDriver creates a new WorkspaceManagerPrescale
func NewWorkspaceManagerPrescaleDriver(config WorkspaceManagerPrescaleDriverConfig, controller Controller) (*WorkspaceManagerPrescaleDriver, error) {
	if config.GhostOwner == "" {
		config.GhostOwner = DefaultGhostOwner
	}
	if config.Renewal.Percentage < 0 && 100 < config.Renewal.Percentage {
		return nil, xerrors.Errorf("renewal.percentage must be between 0 and 100 (inclusive)")
	}

	conn, err := grpc.Dial(config.WorkspaceManagerAddr, grpc.WithInsecure())
	if err != nil {
		return nil, xerrors.Errorf("cannot connect to ws-manager: %w", err)
	}
	return &WorkspaceManagerPrescaleDriver{
		Config:     config,
		Client:     api.NewWorkspaceManagerClient(conn),
		conn:       conn,
		Controller: controller,
		stop:       make(chan struct{}),
	}, nil
}

// WorkspaceManagerPrescaleDriver implements a prescale driver using ws-manager's ghost pods
type WorkspaceManagerPrescaleDriver struct {
	Config WorkspaceManagerPrescaleDriverConfig

	Client api.WorkspaceManagerClient
	conn   *grpc.ClientConn

	Controller Controller

	stop chan struct{}
	once sync.Once
}

type workspaceStatus struct {
	Count              WorkspaceCount
	DeletionCandidates []string
}

// Run runs the prescale driver until Stop() is called
func (wspd *WorkspaceManagerPrescaleDriver) Run() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	statusChan := make(chan workspaceStatus)
	go func() {
		for {
			err := wspd.maintainWorkspaceStatus(ctx, statusChan)
			if err == context.Canceled {
				return
			}
			if err != nil {
				log.WithError(err).Error("cannot maintain workspace count")
				time.Sleep(1 * time.Second)
			}
		}
	}()

	var renewal <-chan time.Time
	if wspd.Config.Renewal.Percentage > 0 && wspd.Config.Renewal.Interval > 0 {
		t := time.NewTicker(time.Duration(wspd.Config.Renewal.Interval))
		defer t.Stop()
		renewal = t.C
	}
	houseKeeping := time.NewTicker(1 * time.Minute)
	defer houseKeeping.Stop()

	var (
		counts         = make(chan WorkspaceCount)
		status         workspaceStatus
		startingGhosts = make(map[string]time.Time)
	)

	delta := wspd.Controller.Control(ctx, counts)
	for {
		select {
		case <-ctx.Done():
			return
		case status = <-statusChan:
			for _, id := range status.DeletionCandidates {
				delete(startingGhosts, id)
			}
			status.Count.Ghost += len(startingGhosts)
			counts <- status.Count
			log.WithField("counts", counts).Debug("status update")
		case <-houseKeeping.C:
			for id, t1 := range startingGhosts {
				if time.Since(t1) < maxGhostStartTime {
					continue
				}
				delete(startingGhosts, id)
				status.Count.Ghost--
			}
		case <-renewal:
			d := int(float64(len(status.DeletionCandidates)) * float64(wspd.Config.Renewal.Percentage) * 0.01)
			if d == 0 {
				continue
			}
			if d > len(status.DeletionCandidates) {
				d = len(status.DeletionCandidates)
			}
			log.WithField("count", d).Info("attempting to renew ghost workspaces")
			err := wspd.stopGhostWorkspaces(ctx, status.DeletionCandidates[:d])
			if err != nil {
				log.WithError(err).Error("cannot stop ghost workspaces during renewal")
				continue
			}
			ids, err := wspd.startGhostWorkspaces(ctx, d, status)
			if err != nil {
				log.WithError(err).Error("cannot start ghost workspaces during renewal")
				continue
			}
			for _, id := range ids {
				startingGhosts[id] = time.Now()
				status.Count.Ghost++
			}
		case d := <-delta:
			if d == 0 {
				continue
			}
			if wspd.Config.ReactionDelay > 0 {
				time.Sleep(time.Duration(wspd.Config.ReactionDelay))
			}
			var (
				err error
				ids []string
			)
			if d < 0 {
				d *= -1
				if d > len(status.DeletionCandidates) {
					d = len(status.DeletionCandidates)
				}
				err = wspd.stopGhostWorkspaces(ctx, status.DeletionCandidates[:d])
			}
			if d > 0 {
				ids, err = wspd.startGhostWorkspaces(ctx, d, status)
			}
			if err != nil {
				log.WithError(err).Error("failed to realise ghost workspace delta")
				continue
			}
			for _, id := range ids {
				startingGhosts[id] = time.Now()
				status.Count.Ghost++
			}
			log.WithField("delta", d).WithField("started", len(ids)).Info("controller requested ghost workspaces")
		}
	}
}

func (wspd *WorkspaceManagerPrescaleDriver) startGhostWorkspaces(ctx context.Context, count int, status workspaceStatus) (ids []string, err error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	ids = make([]string, count)
	for i := 0; i < count; i++ {
		if status.Count.Ghost+i > wspd.Config.MaxGhostWorkspaces {
			log.WithField("limit", wspd.Config.MaxGhostWorkspaces).Warn("max number of ghost workspace reached")
			return ids, nil
		}

		instanceUUID, err := uuid.NewRandom()
		if err != nil {
			return nil, err
		}
		instanceID := instanceUUID.String()
		ids[i] = instanceID
		metaUUID, err := uuid.NewRandom()
		if err != nil {
			return nil, err
		}
		metaID := metaUUID.String()

		_, err = wspd.Client.StartWorkspace(ctx, &api.StartWorkspaceRequest{
			Type: api.WorkspaceType_GHOST,
			Id:   instanceID,
			Metadata: &api.WorkspaceMetadata{
				MetaId: metaID,
				Owner:  wspd.Config.GhostOwner,
			},
			ServicePrefix: instanceID,
			Spec: &api.StartWorkspaceSpec{
				Admission:        api.AdmissionLevel_ADMIT_OWNER_ONLY,
				Timeout:          "60m",
				CheckoutLocation: "none",
				FeatureFlags: []api.WorkspaceFeatureFlag{
					api.WorkspaceFeatureFlag_REGISTRY_FACADE,
				},
				Git: &api.GitSpec{
					Email:    "none@gitpod.io",
					Username: "gitpod-ghost",
				},
				IdeImage: wspd.Config.IDEImage,
				Initializer: &csapi.WorkspaceInitializer{
					Spec: &csapi.WorkspaceInitializer_Empty{
						Empty: &csapi.EmptyInitializer{},
					},
				},
				WorkspaceLocation: "none",
				WorkspaceImage:    wspd.Config.WorkspaceImage,
			},
		})
		if err != nil {
			return nil, err
		}
	}

	return ids, nil
}

func (wspd *WorkspaceManagerPrescaleDriver) stopGhostWorkspaces(ctx context.Context, ids []string) (err error) {
	defer func() {
		if err != nil {
			err = xerrors.Errorf("cannot stop ghosts: %w", err)
		}
	}()

	for _, id := range ids {
		_, err := wspd.Client.StopWorkspace(ctx, &api.StopWorkspaceRequest{Id: id, Policy: api.StopWorkspacePolicy_NORMALLY})
		if status.Code(err) == codes.NotFound {
			continue
		}
		if err != nil {
			return err
		}
	}

	return nil
}

func (wspd *WorkspaceManagerPrescaleDriver) maintainWorkspaceStatus(ctx context.Context, counts chan<- workspaceStatus) error {
	type workspaceState struct {
		Started time.Time
		Type    api.WorkspaceType
	}

	wss, err := wspd.Client.GetWorkspaces(ctx, &api.GetWorkspacesRequest{})
	if err != nil {
		return err
	}

	state := make(map[string]workspaceState)
	produceStatus := func() workspaceStatus {
		var res workspaceStatus
		res.DeletionCandidates = make([]string, 0, len(state))
		for id, s := range state {
			switch s.Type {
			case api.WorkspaceType_GHOST:
				res.Count.Ghost++
			case api.WorkspaceType_PREBUILD:
				res.Count.Prebuild++
			case api.WorkspaceType_REGULAR:
				res.Count.Regular++
			}
			res.DeletionCandidates = append(res.DeletionCandidates, id)
		}

		sort.Slice(res.DeletionCandidates, func(i, j int) bool {
			var (
				ti = state[res.DeletionCandidates[i]].Started
				tj = state[res.DeletionCandidates[j]].Started
			)
			return ti.Before(tj)
		})
		return res
	}

	for _, s := range wss.Status {
		startedAt, err := ptypes.Timestamp(s.Metadata.StartedAt)
		if err != nil {
			log.WithError(err).WithFields(log.OWI(s.Metadata.Owner, s.Metadata.MetaId, s.Id)).Warn("cannot convert startedAt timestamp")
			startedAt = time.Now()
		}
		state[s.Id] = workspaceState{
			Started: startedAt,
			Type:    s.Spec.Type,
		}
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case counts <- produceStatus():
	}

	sub, err := wspd.Client.Subscribe(ctx, &api.SubscribeRequest{})
	if err != nil {
		return err
	}
	for {
		resp, err := sub.Recv()
		if err != nil {
			return err
		}
		s := resp.GetStatus()
		if s == nil {
			continue
		}

		_, known := state[s.Id]
		if known && s.Phase == api.WorkspacePhase_STOPPED {
			delete(state, s.Id)
		} else if !known && s.Phase == api.WorkspacePhase_PENDING {
			startedAt, err := ptypes.Timestamp(s.Metadata.StartedAt)
			if err != nil {
				log.WithError(err).WithFields(log.OWI(s.Metadata.Owner, s.Metadata.MetaId, s.Id)).Warn("cannot convert startedAt timestamp")
				startedAt = time.Now()
			}
			state[s.Id] = workspaceState{
				Started: startedAt,
				Type:    s.Spec.Type,
			}
			state[s.Id] = workspaceState{
				Started: startedAt,
				Type:    s.Spec.Type,
			}
		} else {
			continue
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case counts <- produceStatus():
		}
	}
}

// Stop stops the driver
func (wspd *WorkspaceManagerPrescaleDriver) Stop() {
	wspd.once.Do(func() {
		close(wspd.stop)
		wspd.conn.Close()
	})
}
