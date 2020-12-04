// Copyright (c) 2020 TypeFox GmbH. All rights reserved.
// Licensed under the Gitpod Enterprise Source Code License,
// See License.enterprise.txt in the project root folder.

package scaler_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/gitpod-io/gitpod/ws-scheduler/pkg/scaler"
	"github.com/google/go-cmp/cmp"
)

func TestConstantSetpointController(t *testing.T) {
	type Step struct {
		Input    int
		Expected int
	}
	tests := []struct {
		Name   string
		Target int
		Steps  []Step
	}{
		{
			Name:   "0 target",
			Target: 0,
			Steps: []Step{
				{0, 0},
				{10, -10},
			},
		},
		{
			Name:   "10 target",
			Target: 10,
			Steps: []Step{
				{0, 10},
				{10, 0},
				{5, 5},
			},
		},
	}

	for _, test := range tests {
		t.Run(test.Name, func(t *testing.T) {
			c := &scaler.ConstantSetpointController{Target: test.Target}
			inc := make(chan scaler.WorkspaceCount)

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			out := c.Control(ctx, inc)
			for i, s := range test.Steps {
				t.Run(fmt.Sprintf("step_%03d", i), func(t *testing.T) {
					inc <- scaler.WorkspaceCount{Ghost: s.Input}
					act := <-out

					if diff := cmp.Diff(s.Expected, act); diff != "" {
						t.Errorf("unexpected result (-want +got):\n%s", diff)
					}
				})
			}
		})
	}
}

func TestTimedFunctionController(t *testing.T) {
	type Step struct {
		Time   time.Time
		Target int
	}
	tests := []struct {
		Name  string
		F     scaler.SetpointOverTime
		Steps []Step
	}{
		{
			Name: "linear",
			F: func(t time.Time) (setpoint int) {
				return int(t.Sub(time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)).Seconds())
			},
			Steps: []Step{
				{Time: time.Date(2020, 1, 1, 5, 0, 0, 0, time.UTC), Target: 18000},
				{Time: time.Date(2020, 1, 1, 6, 0, 0, 0, time.UTC), Target: 21600},
				{Time: time.Date(2020, 1, 1, 7, 0, 0, 0, time.UTC), Target: 25200},
				{Time: time.Date(2020, 1, 1, 8, 0, 0, 0, time.UTC), Target: 28800},
			},
		},
	}

	for _, test := range tests {
		t.Run(test.Name, func(t *testing.T) {
			tick := make(chan time.Time)
			tchan := make(chan int)
			var newTickerCalled bool

			c := &scaler.TimedFunctionController{
				F: test.F,
				NewTicker: func() (c <-chan time.Time, stop func()) {
					newTickerCalled = true
					return tick, func() {}
				},
				SetpointChanged: func(newTarget int) {
					tchan <- newTarget
				},
			}
			inc := make(chan scaler.WorkspaceCount)

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			c.Control(ctx, inc)

			if !newTickerCalled {
				t.Fatal("NewTicker was not called")
			}

			for i, s := range test.Steps {
				t.Run(fmt.Sprintf("step_%03d", i), func(t *testing.T) {
					tick <- s.Time
					act := <-tchan

					if diff := cmp.Diff(s.Target, act); diff != "" {
						t.Errorf("unexpected result (-want +got):\n%s", diff)
					}
				})
			}
		})
	}
}

func TestSwitchedSetpointController(t *testing.T) {
	p := func(tod string) scaler.TimeOfDay {
		res, err := time.Parse("15:04:05", tod)
		if err != nil {
			t.Fatal(err)
		}
		return scaler.TimeOfDay(res)
	}

	type Step struct {
		Time   scaler.TimeOfDay
		Target int
	}
	tests := []struct {
		Name            string
		DefaultSetpoint int
		Setpoints       []scaler.SwitchedSetpoint
		Steps           []Step
	}{
		{
			Name:            "basic switchover",
			DefaultSetpoint: 2,
			Setpoints: []scaler.SwitchedSetpoint{
				{Time: p("08:00:00"), Setpoint: 10},
				{Time: p("12:00:00"), Setpoint: 5},
				{Time: p("18:00:00"), Setpoint: 1},
			},
			Steps: []Step{
				{Time: p("05:00:00"), Target: 2},
				{Time: p("09:00:00"), Target: 10},
				{Time: p("10:00:00"), Target: 10},
				{Time: p("12:00:00"), Target: 5},
				{Time: p("13:00:00"), Target: 5},
				{Time: p("19:00:00"), Target: 1},
			},
		},
		{
			Name:            "next day",
			DefaultSetpoint: 2,
			Setpoints: []scaler.SwitchedSetpoint{
				{Time: p("08:00:00"), Setpoint: 10},
			},
			Steps: []Step{
				{Time: p("05:00:00"), Target: 2},
				{Time: p("09:00:00"), Target: 10},
				{Time: p("05:00:00"), Target: 2},
			},
		},
	}

	for _, test := range tests {
		t.Run(test.Name, func(t *testing.T) {
			c, err := scaler.NewSwitchedSetpointController(test.DefaultSetpoint, test.Setpoints)
			if err != nil {
				t.Fatal(err)
			}

			var (
				tick            = make(chan time.Time)
				newTickerCalled bool
				schan           = make(chan int)
			)
			c.NewTicker = func() (c <-chan time.Time, stop func()) {
				newTickerCalled = true
				return tick, func() {}
			}
			c.SetpointChanged = func(old, new int) {
				schan <- new
			}
			c.Now = func() time.Time {
				return time.Time(p("00:00:00"))
			}

			inc := make(chan scaler.WorkspaceCount)

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			c.Control(ctx, inc)

			if !newTickerCalled {
				t.Fatal("NewTicker was not called")
			}

			for i, s := range test.Steps {
				t.Run(fmt.Sprintf("step_%03d_%s", i, time.Time(s.Time).String()), func(t *testing.T) {
					tick <- time.Time(s.Time)
					act := <-schan

					if diff := cmp.Diff(s.Target, act); diff != "" {
						t.Errorf("unexpected result (-want +got):\n%s", diff)
					}
				})
			}
		})
	}
}
