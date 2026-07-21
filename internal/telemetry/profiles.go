package telemetry

import (
	"time"

	"github.com/grafana/pyroscope-go"
)

// NewProfiler continuously sends runtime profiles to a Pyroscope server.
func NewProfiler(serverAddress, applicationName, environment string) (func() error, error) {
	if serverAddress == "" {
		return func() error { return nil }, nil
	}

	profiler, err := pyroscope.Start(pyroscope.Config{
		ApplicationName: applicationName,
		ServerAddress:   serverAddress,
		UploadRate:      10 * time.Second,
		Tags: map[string]string{
			"deployment_environment": environment,
		},
		ProfileTypes: []pyroscope.ProfileType{
			pyroscope.ProfileCPU,
			pyroscope.ProfileAllocObjects,
			pyroscope.ProfileAllocSpace,
			pyroscope.ProfileInuseObjects,
			pyroscope.ProfileInuseSpace,
			pyroscope.ProfileGoroutines,
			pyroscope.ProfileMutexCount,
			pyroscope.ProfileMutexDuration,
			pyroscope.ProfileBlockCount,
			pyroscope.ProfileBlockDuration,
		},
	})
	if err != nil {
		return nil, err
	}
	return profiler.Stop, nil
}
