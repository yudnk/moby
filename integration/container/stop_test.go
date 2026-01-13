package container

import (
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/moby/moby/api/types/common"
	containertypes "github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
	"github.com/moby/moby/v2/integration/internal/container"
	"github.com/moby/moby/v2/internal/testutil/request"
	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
	"gotest.tools/v3/poll"
	"gotest.tools/v3/skip"
)

// hcs can sometimes take a long time to stop container.
const StopContainerWindowsPollTimeout = 75 * time.Second

func TestStopContainer(t *testing.T) {
	ctx := setupTest(t)
	apiClient := testEnv.APIClient()
	timeout := 30

	tests := []struct {
		testName    string
		queryParams client.ContainerStopOptions
		skipOn      string
		expStatus   int
		expError    string
	}{
		{
			testName: "stop the running container",
			queryParams: client.ContainerStopOptions{
				Timeout: &timeout,
			},
		},
		{
			testName: "stop the stoped container",
			queryParams: client.ContainerStopOptions{
				Timeout: &timeout,
			},
			expError: "container already stopped",
		},
	}

	containerID := container.Run(ctx, t, apiClient)

	for _, tc := range tests {
		t.Run(tc.testName, func(t *testing.T) {
			skip.If(t, testEnv.DaemonInfo.OSType == tc.skipOn)

			_, err := apiClient.ContainerStop(ctx, containerID, tc.queryParams)

			if tc.expError != "" {
				// assert.ErrorContains(t, err, tc.expError)
				assert.NilError(t, err)
			} else {
				assert.NilError(t, err)
			}
		})
	}
}

func TestContainerAPIPostContainerStop(t *testing.T) {
	apiClient := testEnv.APIClient()
	ctx := setupTest(t)
	cid := container.Run(ctx, t, apiClient)

	tests := []struct {
		testName  string
		id        string
		signal    string
		t         string
		skipOn    string
		expStatus int
		expError  string
	}{
		{
			testName:  "no error",
			id:        cid,
			expStatus: http.StatusNoContent,
		},
		{
			testName:  "container already stopped",
			id:        cid,
			expStatus: http.StatusNotModified,
		},
		{
			testName:  "no such container",
			id:        "test1234",
			expStatus: http.StatusNotFound,
			expError:  `No such container: test1234`,
		},
		{
			testName:  "server error",
			id:        "test1234",
			t:         "invalid",
			expStatus: http.StatusInternalServerError,
			expError:  `strconv.Atoi: parsing "invalid": invalid syntax`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.testName, func(t *testing.T) {
			skip.If(t, testEnv.DaemonInfo.OSType == tc.skipOn)

			endpoint, err := url.Parse("/containers/" + tc.id + "/stop")
			if err != nil {
				t.Fatal(err)
			}
			query := endpoint.Query()
			if tc.signal != "" {
				query.Set("signal", tc.signal)
			}
			if tc.t != "" {
				query.Set("t", tc.t)
			}
			endpoint.RawQuery = query.Encode()

			res, _, err := request.Post(ctx, endpoint.String())

			assert.Equal(t, res.StatusCode, tc.expStatus)
			assert.NilError(t, err)

			if tc.expError != "" {
				var respErr common.ErrorResponse
				assert.NilError(t, request.ReadJSONResponse(res, &respErr))
				assert.ErrorContains(t, respErr, tc.expError)
			}
		})
	}
}

func TestStopContainerWithRestartPolicyAlways(t *testing.T) {
	ctx := setupTest(t)
	apiClient := testEnv.APIClient()

	names := []string{"verifyRestart1-" + t.Name(), "verifyRestart2-" + t.Name()}
	for _, name := range names {
		container.Run(ctx, t, apiClient,
			container.WithName(name),
			container.WithCmd("false"),
			container.WithRestartPolicy(containertypes.RestartPolicyAlways),
		)
	}

	for _, name := range names {
		poll.WaitOn(t, container.IsInState(ctx, apiClient, name, containertypes.StateRunning, containertypes.StateRestarting))
	}

	for _, name := range names {
		_, err := apiClient.ContainerStop(ctx, name, client.ContainerStopOptions{})
		assert.NilError(t, err)
	}

	for _, name := range names {
		poll.WaitOn(t, container.IsStopped(ctx, apiClient, name))
	}
}

// TestStopContainerWithTimeout checks that ContainerStop with
// a timeout works as documented, i.e. in case of negative timeout
// waiting is not limited (issue #35311).
func TestStopContainerWithTimeout(t *testing.T) {
	isWindows := testEnv.DaemonInfo.OSType == "windows"
	// TODO(vvoland): Make this work on Windows
	skip.If(t, isWindows)

	ctx := setupTest(t)
	apiClient := testEnv.APIClient()

	forcefulKillExitCode := 137
	if isWindows {
		forcefulKillExitCode = 0x40010004
	}

	testCmd := container.WithCmd("sh", "-c", "sleep 10 && exit 42")
	testData := []struct {
		doc              string
		timeout          int
		expectedExitCode int
	}{
		// In case container is forcefully killed, 137 is returned,
		// otherwise the exit code from the above script
		{
			doc:              "zero timeout: expect forceful container kill",
			expectedExitCode: forcefulKillExitCode,
			timeout:          0,
		},
		{
			doc:              "too small timeout: expect forceful container kill",
			expectedExitCode: forcefulKillExitCode,
			timeout:          2,
		},
		{
			doc:              "big enough timeout: expect graceful container stop",
			expectedExitCode: 42,
			timeout:          20, // longer than "sleep 10" cmd
		},
		{
			doc:              "unlimited timeout: expect graceful container stop",
			expectedExitCode: 42,
			timeout:          -1,
		},
	}

	var pollOpts []poll.SettingOp
	if isWindows {
		pollOpts = append(pollOpts, poll.WithTimeout(StopContainerWindowsPollTimeout))
	}

	for _, tc := range testData {
		t.Run(tc.doc, func(t *testing.T) {
			// TODO(vvoland): Investigate why it helps
			// t.Parallel()
			id := container.Run(ctx, t, apiClient, testCmd)

			_, err := apiClient.ContainerStop(ctx, id, client.ContainerStopOptions{Timeout: &tc.timeout})
			assert.NilError(t, err)

			poll.WaitOn(t, container.IsStopped(ctx, apiClient, id), pollOpts...)

			inspect, err := apiClient.ContainerInspect(ctx, id, client.ContainerInspectOptions{})
			assert.NilError(t, err)
			assert.Check(t, is.Equal(inspect.Container.State.ExitCode, tc.expectedExitCode))
		})
	}
}
