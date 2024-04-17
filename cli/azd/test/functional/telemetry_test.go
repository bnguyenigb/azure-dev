// Copyright (c) Microsoft Corporation. All rights reserved.
// Licensed under the MIT License.

package cli_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/azure/azure-dev/cli/azd/internal/tracing/fields"
	"github.com/azure/azure-dev/cli/azd/pkg/config"
	"github.com/azure/azure-dev/cli/azd/pkg/environment"
	"github.com/azure/azure-dev/cli/azd/pkg/environment/azdcontext"
	"github.com/azure/azure-dev/cli/azd/pkg/osutil"
	"github.com/azure/azure-dev/cli/azd/pkg/project"
	"github.com/azure/azure-dev/cli/azd/test/azdcli"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// Span is the format generated by stdouttrace, which is used by azd when --trace-log-file is specified.
// stdouttrace is not a stable exporter and does not support bidirectional marshaling,
// and thus we have a minimal struct that can be modified when needed.
type Span struct {
	Name        string
	SpanContext SpanContext
	Resource    []Attribute
	Attributes  []Attribute
}

// Like [trace.SpanContext], except uses string representations of IDs.
type SpanContext struct {
	TraceID string
	SpanID  string
}

func (sc *SpanContext) Validate() error {
	_, err := trace.TraceIDFromHex(sc.TraceID)
	if err != nil {
		return err
	}

	_, err = trace.SpanIDFromHex(sc.SpanID)
	if err != nil {
		return err
	}

	return nil
}

type Value struct {
	Type  string
	Value interface{}
}

type Attribute struct {
	Key   string
	Value Value
}

var Sha256Regex = regexp.MustCompile("^[A-Fa-f0-9]{64}$")

// Verifies telemetry usage data generated for simple commands, such as when environments are created.
func Test_CLI_Telemetry_UsageData_Simple_Command(t *testing.T) {
	// CLI process and working directory are isolated
	t.Parallel()
	ctx, cancel := newTestContext(t)
	defer cancel()

	dir := tempDirWithDiagnostics(t)
	t.Logf("DIR: %s", dir)

	cli := azdcli.NewCLI(t)
	// Always set telemetry opt-inn setting to avoid influence from user settings
	cli.Env = append(os.Environ(), "AZURE_DEV_COLLECT_TELEMETRY=yes")
	cli.WorkingDirectory = dir

	envName := randomEnvName()

	err := copySample(dir, "storage")
	require.NoError(t, err, "failed expanding sample")

	traceFilePath := filepath.Join(dir, "trace.json")

	_, err = cli.RunCommand(ctx, "env", "new", envName, "--trace-log-file", traceFilePath)
	require.NoError(t, err)
	fmt.Printf("envName: %s\n", envName)

	traceContent, err := os.ReadFile(traceFilePath)
	require.NoError(t, err)

	scanner := bufio.NewScanner(bytes.NewReader(traceContent))
	usageCmdFound := false
	for scanner.Scan() {
		if scanner.Text() == "" {
			continue
		}

		var span Span
		err = json.Unmarshal(scanner.Bytes(), &span)
		require.NoError(t, err)

		verifyResource(t, cli.Env, span.Resource)
		if strings.HasPrefix(span.Name, "cmd.") {
			usageCmdFound = true
			m := attributesMap(span.Attributes)
			require.Contains(t, m, fields.EnvNameKey)
			require.Equal(t, fields.CaseInsensitiveHash(envName), m[fields.EnvNameKey])

			require.Contains(t, m, fields.CmdFlags)
			require.ElementsMatch(t, m[fields.CmdFlags], []string{"trace-log-file"})

			// env new provides a single position argument.
			require.Contains(t, m, fields.CmdArgsCount)
			require.Equal(t, float64(1), m[fields.CmdArgsCount])
		}
	}

	require.True(t, usageCmdFound)
}

// Verifies telemetry usage data generated when environments and projects are loaded.
func Test_CLI_Telemetry_UsageData_EnvProjectLoad(t *testing.T) {
	// CLI process and working directory are isolated
	ctx, cancel := newTestContext(t)
	defer cancel()

	dir := tempDirWithDiagnostics(t)
	t.Logf("DIR: %s", dir)

	cli := azdcli.NewCLI(t)
	// Always set telemetry opt-inn setting to avoid influence from user settings
	cli.Env = append(os.Environ(), "AZURE_DEV_COLLECT_TELEMETRY=yes")
	cli.WorkingDirectory = dir

	envName := randomEnvName()

	err := copySample(dir, "restoreapp")
	require.NoError(t, err, "failed expanding sample")

	traceFilePath := filepath.Join(dir, "trace.json")

	_, err = cli.RunCommandWithStdIn(ctx, stdinForInit(envName), "init")
	require.NoError(t, err)

	_, err = cli.RunCommand(ctx, "env", "set", "AZURE_SUBSCRIPTION_ID", cfg.SubscriptionID)
	require.NoError(t, err)

	_, err = cli.RunCommand(ctx, "restore", "csharpapptest", "--trace-log-file", traceFilePath)
	require.NoError(t, err)

	traceContent, err := os.ReadFile(traceFilePath)
	require.NoError(t, err)

	projectContent, err := samples.ReadFile(samplePath("restoreapp", "azure.yaml"))
	require.NoError(t, err)
	projConfig, err := project.Parse(ctx, string(projectContent))
	require.NoError(t, err)

	scanner := bufio.NewScanner(bytes.NewReader(traceContent))
	usageCmdFound := false
	for scanner.Scan() {
		if scanner.Text() == "" {
			continue
		}

		var span Span
		err = json.Unmarshal(scanner.Bytes(), &span)
		require.NoError(t, err)

		verifyResource(t, cli.Env, span.Resource)
		if span.Name == "cmd.restore" {
			usageCmdFound = true
			m := attributesMap(span.Attributes)
			require.Contains(t, m, fields.SubscriptionIdKey)
			require.Equal(t, getEnvSubscriptionId(t, dir, envName), m[fields.SubscriptionIdKey])

			templateAndVersion := strings.Split(projConfig.Metadata.Template, "@")
			require.Len(t, templateAndVersion, 2)
			require.Contains(t, m, fields.ProjectTemplateIdKey)
			require.Equal(t, fields.CaseInsensitiveHash(templateAndVersion[0]), m[fields.ProjectTemplateIdKey])

			require.Contains(t, m, fields.ProjectTemplateVersionKey)
			require.Equal(t, fields.CaseInsensitiveHash(templateAndVersion[1]), m[fields.ProjectTemplateVersionKey])

			require.Contains(t, m, fields.ProjectNameKey)
			require.Equal(t, fields.CaseInsensitiveHash(projConfig.Name), m[fields.ProjectNameKey])

			require.Contains(t, m, fields.EnvNameKey)
			require.Equal(t, fields.CaseInsensitiveHash(envName), m[fields.EnvNameKey])

			hosts := []string{}
			languages := []string{}
			for _, svc := range projConfig.Services {
				hosts = append(hosts, string(svc.Host))
				languages = append(languages, string(svc.Language))
			}
			require.Contains(t, m, fields.ProjectServiceHostsKey)
			require.ElementsMatch(t, m[fields.ProjectServiceHostsKey], hosts)

			require.Contains(t, m, fields.ProjectServiceLanguagesKey)
			require.ElementsMatch(t, m[fields.ProjectServiceLanguagesKey], languages)

			require.Contains(t, m, fields.CmdFlags)
			require.ElementsMatch(t, m[fields.CmdFlags], []string{"trace-log-file"})

			require.Contains(t, m, fields.CmdArgsCount)
			require.Equal(t, float64(1), m[fields.CmdArgsCount])
		}
	}
	require.True(t, usageCmdFound)
}

// Verifies telemetry behavior for nested commands, such as ones invoked from `up`.
func Test_CLI_Telemetry_NestedCommands(t *testing.T) {
	// CLI process and working directory are isolated
	ctx, cancel := newTestContext(t)
	defer cancel()

	dir := tempDirWithDiagnostics(t)
	t.Logf("DIR: %s", dir)

	traceFilePath := filepath.Join(dir, "trace.json")

	cli := azdcli.NewCLI(t)
	// Always set telemetry opt-inn setting to avoid influence from user settings
	cli.Env = append(os.Environ(), "AZURE_DEV_COLLECT_TELEMETRY=yes")

	// set environment modifier
	cli.Env = append(cli.Env, "AZURE_DEV_USER_AGENT=azure_app_space_portal:v1.0.0")
	cli.WorkingDirectory = dir

	envName := randomEnvName()

	_, err := cli.RunCommandWithStdIn(
		ctx,
		// Choose the default minimal template
		"Select a template\n\n"+stdinForInit(envName),
		"init")
	require.NoError(t, err)

	// Remove infra folder to avoid lengthy Azure operations while asserting the intended telemetry behavior.
	// The current behavior is that `azd provision` will fail when trying to read the nonexistent bicep folder.
	infraPath := filepath.Join(dir, "infra")
	require.NoError(t, os.RemoveAll(infraPath))

	// We do require that infra folder exist, however, so put it back with a module which will throw during provisioning.
	require.NoError(t, os.MkdirAll(infraPath, osutil.PermissionDirectoryOwnerOnly))
	// empty main.bicep but with no parameters file.
	file, err := os.Create(filepath.Join(infraPath, "main.bicep"))
	require.NoError(t, err)
	defer file.Close()

	_, err = cli.RunCommandWithStdIn(ctx, stdinForProvision(), "up", "--trace-log-file", traceFilePath)
	require.Error(t, err)

	traceContent, err := os.ReadFile(traceFilePath)
	require.NoError(t, err)

	scanner := bufio.NewScanner(bytes.NewReader(traceContent))
	// In order of observed events: package -> provision -> up
	packageCmdFound := false
	provisionCmdFound := false
	upCmdFound := false
	traceId := ""
	for scanner.Scan() {
		if scanner.Text() == "" {
			continue
		}

		var span Span
		err = json.Unmarshal(scanner.Bytes(), &span)
		require.NoError(t, err)

		verifyResource(t, cli.Env, span.Resource)
		if !strings.HasPrefix(span.Name, "cmd.") {
			continue
		}

		if !packageCmdFound {
			require.Equal(t, "cmd.package", span.Name)
			packageCmdFound = true
			require.NoError(t, span.SpanContext.Validate(), "invalid span context")
			// set the traceID
			traceId = span.SpanContext.TraceID

			m := attributesMap(span.Attributes)
			require.Contains(t, m, fields.EnvNameKey)
			require.Equal(t, fields.CaseInsensitiveHash(envName), m[fields.EnvNameKey])

			require.Contains(t, m, fields.CmdEntry)
			require.Equal(t, "cmd.up", m[fields.CmdEntry])

			require.Contains(t, m, fields.CmdFlags)
			require.ElementsMatch(t, []string{"all", "trace-log-file"}, m[fields.CmdFlags])
		} else if !provisionCmdFound {
			require.Equal(t, "cmd.provision", span.Name)
			provisionCmdFound = true
			require.Equal(t, traceId, span.SpanContext.TraceID, "commands do not share a traceID")

			m := attributesMap(span.Attributes)
			require.Contains(t, m, fields.SubscriptionIdKey)
			require.Equal(t, getEnvSubscriptionId(t, dir, envName), m[fields.SubscriptionIdKey])

			require.Contains(t, m, fields.EnvNameKey)
			require.Equal(t, fields.CaseInsensitiveHash(envName), m[fields.EnvNameKey])

			require.Contains(t, m, fields.CmdEntry)
			require.Equal(t, "cmd.up", m[fields.CmdEntry])

			require.Contains(t, m, fields.CmdFlags)
			require.ElementsMatch(t, []string{"trace-log-file"}, m[fields.CmdFlags])
		} else if !upCmdFound {
			require.Equal(t, "cmd.up", span.Name)
			upCmdFound = true
			require.Equal(t, traceId, span.SpanContext.TraceID, "commands do not share a traceID")

			m := attributesMap(span.Attributes)
			require.Contains(t, m, fields.SubscriptionIdKey)
			require.Equal(t, getEnvSubscriptionId(t, dir, envName), m[fields.SubscriptionIdKey])

			require.Contains(t, m, fields.EnvNameKey)
			require.Equal(t, fields.CaseInsensitiveHash(envName), m[fields.EnvNameKey])

			require.Contains(t, m, fields.CmdEntry)
			require.Equal(t, "cmd.up", m[fields.CmdEntry])

			require.Contains(t, m, fields.CmdFlags)
			require.ElementsMatch(t, []string{"trace-log-file"}, m[fields.CmdFlags])

			require.Contains(t, m, fields.CmdArgsCount)
			require.Equal(t, float64(0), m[fields.CmdArgsCount])
		}
	}
	require.True(t, packageCmdFound, "cmd.package not found")
	require.True(t, provisionCmdFound, "cmd.provision not found")
	require.True(t, upCmdFound, "cmd.up not found")
}

func attributesMap(attributes []Attribute) map[attribute.Key]interface{} {
	m := map[attribute.Key]interface{}{}
	for _, attrib := range attributes {
		m[attribute.Key(attrib.Key)] = attrib.Value.Value
	}

	return m
}

func getEnvSubscriptionId(t *testing.T, dir string, envName string) string {
	azdCtx := azdcontext.NewAzdContextWithDirectory(dir)
	localDataStore := environment.NewLocalFileDataStore(azdCtx, config.NewFileConfigManager(config.NewManager()))
	env, err := localDataStore.Get(context.Background(), envName)
	require.NoError(t, err)

	return env.GetSubscriptionId()
}

func verifyResource(
	t *testing.T,
	cmdEnv []string,
	attributes []Attribute) {
	m := attributesMap(attributes)

	require.Contains(t, m, fields.MachineIdKey)
	machineId, ok := m[fields.MachineIdKey].(string)
	require.True(t, ok, "expected machine ID to be string type")
	isSha256 := Sha256Regex.MatchString(machineId)
	_, err := uuid.Parse(machineId)
	isUuid := err == nil
	require.True(t, isSha256 || isUuid, "invalid machine ID format. expected sha256 or uuid")

	require.Contains(t, m, fields.ServiceVersionKey)
	require.Equal(t, m[fields.ServiceVersionKey], getExpectedVersion(t))

	require.Contains(t, m, fields.ServiceVersionKey)
	require.Equal(t, m[fields.ServiceNameKey], fields.ServiceNameAzd)

	require.Contains(t, m, fields.ExecutionEnvironmentKey)

	env := ""
	if os.Getenv("BUILD_BUILDID") != "" {
		env = fields.EnvAzurePipelines
		require.Regexp(t, regexp.MustCompile("^"+fields.EnvAzurePipelines), m[fields.ExecutionEnvironmentKey])
	} else if os.Getenv("GITHUB_RUN_ID") != "" {
		env = fields.EnvGitHubActions
	}

	if env != "" {
		// basic regex that matches a very simple expression (not the entire grammar):
		// env followed by an optional (;modifier)
		require.Regexp(t, regexp.MustCompile("^"+env+"(;\\w)?"), m[fields.ExecutionEnvironmentKey])
	}

	for _, env := range cmdEnv {
		if strings.HasPrefix(env, "AZURE_DEV_USER_AGENT=") && strings.Contains(env, "azure_app_space_portal") {
			require.Contains(t, m[fields.ExecutionEnvironmentKey], ";"+fields.EnvModifierAzureSpace)
		}
	}

	require.Contains(t, m, fields.OSTypeKey)
	require.Contains(t, m, fields.OSVersionKey)
	require.Contains(t, m, fields.HostArchKey)
	require.Contains(t, m, fields.ProcessRuntimeVersionKey)
}
