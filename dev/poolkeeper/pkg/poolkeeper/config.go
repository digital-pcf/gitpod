// Copyright (c) 2020 TypeFox GmbH. All rights reserved.
// Licensed under the Gitpod Enterprise Source Code License,
// See License.enterprise.txt in the project root folder.

package poolkeeper

import "time"

// Config is the poolkeeper config
type Config struct {
	// NodePoolAnnotation specifies which annotation on the Node says to which NodePool it belongs to
	NodePoolAnnotation string `json:"nodePoolAnnotation"`

	// Interval configures how often we check and adjust the configured config in the cluster
	Interval time.Duration `json:"nodePoolIdentityAnnotation"`

	// NodePools allows to configure each NodePool individually
	NodePools []*NodePoolConfig `json:"nodePools"`
}

// NodePoolConfig configures which state we want to monitor on which NodePool
type NodePoolConfig struct {
	// Name identifies the NodePool
	Name string `json:"name"`

	// KeepOut configures which pods should be kept out of the NodePool
	KeepOut *KeepOutConfig `json:"keepOut,omitempty"`
}

// KeepOutConfig specifies pods that should not be running on the specified NodePool - and thus be moved by poolkeeper
type KeepOutConfig struct {
	// Namespace says whose namespace's pod should not be running on this NodePool
	Namespace string `json:"namespace"`
}
