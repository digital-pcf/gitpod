// Copyright (c) 2020 TypeFox GmbH. All rights reserved.
// Licensed under the Gitpod Enterprise Source Code License,
// See License.enterprise.txt in the project root folder.

package scaler

import (
	"context"
	"sort"
	"time"

	"github.com/gitpod-io/gitpod/common-go/log"
	"golang.org/x/xerrors"
)

// WorkspaceCount contains the current counts of running workspaces by type.
type WorkspaceCount struct {
	Regular  int
	Prebuild int
	Ghost    int
}

// Controller encapsulates prescaling strategies
type Controller interface {
	// Control starts this controller which is expected to run until the
	// context is canceled. Ths workspaceCount channel provides updates whenever the
	// curent, regular workspace count changes.
	//
	// Writing to the returned ghost delta channel will reserve as many workspace
	// slots as was written, i.e. `ghostDelta <- 5` will reserve an additional five slots.
	// Writing a negative number will free up reserved slots. Attempting to free up more slots
	// than what's currently reserved will stop at zero. For example:
	//    // starting with zero reserved slots
	//    ghostDelta <- 10
	//    ghostDelta <- -20
	//    ghostDelta <- 5
	//    // we now have 5 slots, not -5
	//
	// When the context is canceled and the controller stops, the slotCount channel must be closed.
	Control(ctx context.Context, workspaceCount <-chan WorkspaceCount) (ghostDelta <-chan int)
}

// ControllerConfig configures the controller
type ControllerConfig struct {
	Kind ControllerType `json:"kind"`

	Constant struct {
		Setpoint int `json:"setpoint"`
	} `json:"constant"`
	SwitchedConstant struct {
		DefaultSetpoint int                `json:"default"`
		Setpoints       []SwitchedSetpoint `json:"setpoints"`
	} `json:"switchedConstant"`
}

// ControllerType names a kind of controller
type ControllerType string

const (
	// ControllerConstantTarget creates a FixedSlotCount controller
	ControllerConstantTarget ControllerType = "constant"

	// ControllerSwitchedConstantTargets switches setpoints over time
	ControllerSwitchedConstantTargets ControllerType = "switchedConstant"
)

// NewController produces a new controller from configuration
func NewController(c ControllerConfig) (Controller, error) {
	switch c.Kind {
	case ControllerConstantTarget:
		return &ConstantSetpointController{Target: c.Constant.Setpoint}, nil
	case ControllerSwitchedConstantTargets:
		return NewSwitchedSetpointController(c.SwitchedConstant.DefaultSetpoint, c.SwitchedConstant.Setpoints)
	default:
		return nil, xerrors.Errorf("unknown controller kind: %v", c.Kind)
	}
}

// ConstantSetpointController maintains a steadily fixed number of ghost workspaces
type ConstantSetpointController struct {
	Target int
}

// Control starts this controller
func (f *ConstantSetpointController) Control(ctx context.Context, workspaceCount <-chan WorkspaceCount) (ghostDelta <-chan int) {
	res := make(chan int)
	go func() {
		defer close(res)
		for {
			select {
			case <-ctx.Done():
				return
			case c := <-workspaceCount:
				diff := f.Target - c.Ghost
				res <- diff
			}
		}
	}()
	return res
}

// TimeOfDay is a time during the day. It unmarshals from JSON as hh:mm:ss string.
type TimeOfDay time.Time

// UnmarshalJSON unmarshales a time of day
func (t *TimeOfDay) UnmarshalJSON(data []byte) error {
	res, err := time.Parse("15:04:05", string(data))
	if err != nil {
		return err
	}
	*t = TimeOfDay(res)
	return nil
}

// SwitchedSetpoint is a setpoint valid from a particular time in the day
type SwitchedSetpoint struct {
	Time     TimeOfDay `json:"time"`
	Setpoint int       `json:"setpoint"`
}

// NewSwitchedSetpointController creates a new SwitchedSetpointController
func NewSwitchedSetpointController(defaultSetpoint int, setpoints []SwitchedSetpoint) (*SwitchedSetpointController, error) {
	if defaultSetpoint < 0 {
		return nil, xerrors.Errorf("defaultSetpoint must be >= 0")
	}

	sort.Slice(setpoints, func(i, j int) bool { return time.Time(setpoints[i].Time).Before(time.Time(setpoints[j].Time)) })
	return &SwitchedSetpointController{
		DefaultSetpoint: defaultSetpoint,
		Setpoints:       setpoints,
		NewTicker:       newDefaultTicker(1 * time.Minute),
		Now:             time.Now,
		SetpointChanged: func(old, new int) {
			if old == new {
				return
			}

			log.WithField("new", new).WithField("old", new).Info("timed function controller target change")
		},
	}, nil
}

// SwitchedSetpointController is like the ConstantSetpointController but with different
// setpoints throughout the day.
type SwitchedSetpointController struct {
	DefaultSetpoint int
	Setpoints       []SwitchedSetpoint

	NewTicker       func() (c <-chan time.Time, stop func())
	SetpointChanged func(old, new int)
	Now             func() time.Time
}

// Control starts this controller
func (c *SwitchedSetpointController) Control(ctx context.Context, workspaceCount <-chan WorkspaceCount) (ghostDelta <-chan int) {
	res := make(chan int)

	setpoint := c.DefaultSetpoint
	if csp := c.findSwitchpoint(c.Now()); csp != nil {
		c.SetpointChanged(setpoint, csp.Setpoint)
		setpoint = csp.Setpoint
	}

	tick, stop := c.NewTicker()
	go func() {
		defer stop()
		defer close(res)
		for {
			select {
			case <-ctx.Done():
				return
			case t := <-tick:
				nsp := -1
				if csp := c.findSwitchpoint(t); csp != nil {
					nsp = csp.Setpoint
				} else {
					nsp = c.DefaultSetpoint
				}
				c.SetpointChanged(setpoint, nsp)
				setpoint = nsp
			case c := <-workspaceCount:
				diff := setpoint - c.Ghost
				res <- diff
			}
		}
	}()
	return res
}

func (c *SwitchedSetpointController) findSwitchpoint(t time.Time) *SwitchedSetpoint {
	if len(c.Setpoints) == 0 {
		return nil
	}

	tod := time.Date(0, 1, 1, t.Hour(), t.Minute(), t.Second(), t.Nanosecond(), t.Location())
	for i, sp := range c.Setpoints {
		spt := time.Time(sp.Time)
		if tod.Equal(spt) {
			return &sp
		}
		if tod.After(spt) {
			continue
		}
		if i == 0 {
			return nil
		}

		return &c.Setpoints[i-1]
	}

	return &c.Setpoints[len(c.Setpoints)-1]
}

func newDefaultTicker(resolution time.Duration) func() (c <-chan time.Time, stop func()) {
	return func() (c <-chan time.Time, stop func()) {
		t := time.NewTicker(resolution)
		return t.C, t.Stop
	}
}

// SetpointOverTime is a function that determines the number of ghost workspaces over time.
type SetpointOverTime func(t time.Time) (setpoint int)

// NewTimedFunctionController produces a new timed function controller
func NewTimedFunctionController(f SetpointOverTime, resolution time.Duration) *TimedFunctionController {
	return &TimedFunctionController{
		F:         f,
		NewTicker: newDefaultTicker(resolution),
		SetpointChanged: func(newTarget int) {
			log.WithField("newTarget", newTarget).Info("timed function controller target change")
		},
	}
}

// TimedFunctionController sample a function over time to set a total amount of ghost workspaces
// that ought to be present at that time.
type TimedFunctionController struct {
	F func(t time.Time) (setpoint int)

	NewTicker       func() (c <-chan time.Time, stop func())
	SetpointChanged func(newTarget int)
}

// Control starts this controller
func (tfc *TimedFunctionController) Control(ctx context.Context, workspaceCount <-chan WorkspaceCount) (ghostDelta <-chan int) {
	res := make(chan int)
	tick, stop := tfc.NewTicker()
	go func() {
		target := 0
		defer close(res)
		defer stop()
		for {
			select {
			case <-ctx.Done():
				return
			case t := <-tick:
				sp := tfc.F(t)
				tfc.SetpointChanged(sp)
			case c := <-workspaceCount:
				diff := target - c.Ghost
				res <- diff
			}
		}
	}()
	return res
}

// SetpointInTime is a sample produced by RenderTimedFunctionController
type SetpointInTime struct {
	T        time.Time
	Setpoint int
}

// RenderSetpointOverTime renders the behaviour of a SetpointOverTime function
func RenderSetpointOverTime(p SetpointOverTime, start, end time.Time, resolution time.Duration) []SetpointInTime {
	var res []SetpointInTime
	for t := start; t.Before(end); t = t.Add(resolution) {
		res = append(res, SetpointInTime{
			T:        t,
			Setpoint: p(t),
		})
	}
	return res
}
