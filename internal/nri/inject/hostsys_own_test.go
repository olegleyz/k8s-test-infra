// Copyright 2026 NVIDIA CORPORATION
// SPDX-License-Identifier: Apache-2.0

package inject

import (
	"reflect"
	"testing"
)

func TestSysfsTargetsOwnSysfs(t *testing.T) {
	cfg := Config{HostOverlayPath: "/var/lib/nvml-mock"}
	sysfs := Mount{Source: "sysfs", Destination: "/sys", Type: "sysfs"}

	tests := []struct {
		name      string
		container Container
		want      []string
	}{
		{
			name:      "named consumer gets its own /sys",
			container: Container{PodLabels: map[string]string{"app": "rdma-shared-dp"}, Mounts: []Mount{sysfs}},
			want:      []string{"/sys"},
		},
		{
			name:      "other privileged containers keep the real /sys",
			container: Container{PodLabels: map[string]string{"app": "kube-multus-ds"}, Mounts: []Mount{sysfs}},
			want:      nil,
		},
		{
			name: "hostPath and own /sys together",
			container: Container{PodLabels: map[string]string{"app": "rdma-shared-dp"}, Mounts: []Mount{
				sysfs, {Source: "/sys", Destination: "/host-sys", Type: "bind"},
			}},
			want: []string{"/host-sys", "/sys"},
		},
		{
			name: "Mokka's own pods are left alone",
			container: Container{PodLabels: map[string]string{"app": "rdma-shared-dp"}, Mounts: []Mount{
				sysfs, {Source: "/var/lib/nvml-mock", Destination: "/opt/nvml-mock", Type: "bind"},
			}},
			want: nil,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := sysfsTargets(cfg, tc.container); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("sysfsTargets() = %v, want %v", got, tc.want)
			}
		})
	}
}
