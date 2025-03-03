package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/pkg/errors"

	"github.com/kubeshop/testkube/contrib/executor/jmeter/pkg/parser"
	"github.com/kubeshop/testkube/contrib/executor/jmeterd/pkg/jmeterenv"
	"github.com/kubeshop/testkube/contrib/executor/jmeterd/pkg/slaves"
	"github.com/kubeshop/testkube/pkg/api/v1/testkube"
	"github.com/kubeshop/testkube/pkg/envs"
	"github.com/kubeshop/testkube/pkg/executor"
	"github.com/kubeshop/testkube/pkg/executor/agent"
	"github.com/kubeshop/testkube/pkg/executor/content"
	"github.com/kubeshop/testkube/pkg/executor/env"
	"github.com/kubeshop/testkube/pkg/executor/output"
	"github.com/kubeshop/testkube/pkg/executor/runner"
	"github.com/kubeshop/testkube/pkg/executor/scraper"
	"github.com/kubeshop/testkube/pkg/executor/scraper/factory"
	"github.com/kubeshop/testkube/pkg/ui"
)

func NewRunner(ctx context.Context, params envs.Params) (*JMeterDRunner, error) {
	output.PrintLogf("%s Preparing test runner", ui.IconTruck)

	var err error
	r := &JMeterDRunner{
		Params: params,
	}

	r.Scraper, err = factory.TryGetScrapper(ctx, params)
	if err != nil {
		return nil, err
	}

	slavesConfigs := executor.SlavesConfigs{}
	if err := json.Unmarshal([]byte(params.SlavesConfigs), &slavesConfigs); err != nil {
		return nil, errors.Wrap(err, "error unmarshalling slaves configs")
	}
	r.SlavesConfigs = slavesConfigs

	return r, nil
}

// JMeterDRunner runner
type JMeterDRunner struct {
	Params        envs.Params
	Scraper       scraper.Scraper
	SlavesConfigs executor.SlavesConfigs
}

var _ runner.Runner = &JMeterDRunner{}

func (r *JMeterDRunner) Run(ctx context.Context, execution testkube.Execution) (result testkube.ExecutionResult, err error) {
	if r.Scraper != nil {
		defer r.Scraper.Close()
	}
	output.PrintEvent(
		fmt.Sprintf("%s Running with config", ui.IconTruck),
		"scraperEnabled", r.Params.ScrapperEnabled,
		"dataDir", r.Params.DataDir,
		"SSL", r.Params.Ssl,
		"endpoint", r.Params.Endpoint,
	)

	envManager := env.NewManagerWithVars(execution.Variables)
	envManager.GetReferenceVars(envManager.Variables)

	path, workingDir, err := content.GetPathAndWorkingDir(execution.Content, r.Params.DataDir)
	if err != nil {
		output.PrintLogf("%s Failed to resolve absolute directory for %s, using the path directly", ui.IconWarning, r.Params.DataDir)
	}

	fileInfo, err := os.Stat(path)
	if err != nil {
		return result, err
	}

	if fileInfo.IsDir() {
		scriptName := execution.Args[len(execution.Args)-1]
		if workingDir != "" {
			path = filepath.Join(r.Params.DataDir, "repo")
			if execution.Content != nil && execution.Content.Repository != nil {
				scriptName = filepath.Join(execution.Content.Repository.Path, scriptName)
			}
		}

		execution.Args = execution.Args[:len(execution.Args)-1]
		output.PrintLogf("%s It is a directory test - trying to find file from the last executor argument %s in directory %s", ui.IconWorld, scriptName, path)

		// sanity checking for test script
		scriptFile := filepath.Join(path, workingDir, scriptName)
		fileInfo, errFile := os.Stat(scriptFile)
		if errors.Is(errFile, os.ErrNotExist) || fileInfo.IsDir() {
			output.PrintLogf("%s Could not find file %s in the directory, error: %s", ui.IconCross, scriptName, errFile)
			return *result.Err(errors.Errorf("could not find file %s in the directory: %v", scriptName, errFile)), nil
		}
		path = scriptFile
	}

	slavesEnvVariables := jmeterenv.ExtractSlaveEnvVariables(envManager.Variables)
	// compose parameters passed to JMeter with -J
	params := make([]string, 0, len(envManager.Variables))
	for _, value := range envManager.Variables {
		if value.Name == jmeterenv.MasterOverrideJvmArgs || value.Name == jmeterenv.MasterAdditionalJvmArgs {
			//Skip JVM ARGS to be appended in the command
			continue
		}
		params = append(params, fmt.Sprintf("-G%s=%s", value.Name, value.Value))

	}

	runPath := r.Params.DataDir
	if workingDir != "" {
		runPath = workingDir
	}

	parentTestFolder := filepath.Join(filepath.Dir(path))
	// Set env plugin env variable to set custom plugin directory
	// with this path custom plugin will be copied to jmeter's plugin directory
	err = os.Setenv("JMETER_PARENT_TEST_FOLDER", parentTestFolder)
	if err != nil {
		output.PrintLogf("%s Failed to set parent test folder directory %s", ui.IconWarning, parentTestFolder)
	}
	// Add user plugins folder in slaves env variables
	slavesEnvVariables["JMETER_PARENT_TEST_FOLDER"] = testkube.NewBasicVariable("JMETER_PARENT_TEST_FOLDER", parentTestFolder)

	outputDir := filepath.Join(runPath, "output")
	// clean output directory it already exists, only useful for local development
	_, err = os.Stat(outputDir)
	if err == nil {
		if err = os.RemoveAll(outputDir); err != nil {
			output.PrintLogf("%s Failed to clean output directory %s", ui.IconWarning, outputDir)
		}
	}
	// recreate output directory with wide permissions so JMeter can create report files
	if err = os.Mkdir(outputDir, 0777); err != nil {
		return *result.Err(errors.Wrapf(err, "error creating directory %s", outputDir)), nil
	}

	jtlPath := filepath.Join(outputDir, "report.jtl")
	reportPath := filepath.Join(outputDir, "report")
	jmeterLogPath := filepath.Join(outputDir, "jmeter.log")
	args := execution.Args
	for i := range args {
		if args[i] == "<runPath>" {
			args[i] = path
		}

		if args[i] == "<jtlFile>" {
			args[i] = jtlPath
		}

		if args[i] == "<reportFile>" {
			args[i] = reportPath
		}

		if args[i] == "<logFile>" {
			args[i] = jmeterLogPath
		}
	}

	slaveClient, err := slaves.NewClient(execution, r.SlavesConfigs, r.Params, slavesEnvVariables)
	if err != nil {
		return *result.WithErrors(errors.Wrap(err, "error creating slaves client")), nil
	}

	//creating slaves provided in SLAVES_COUNT env variable
	slaveMeta, err := slaveClient.CreateSlaves(ctx)
	if err != nil {
		return *result.WithErrors(errors.Wrap(err, "error creating slaves")), nil
	}
	defer slaveClient.DeleteSlaves(ctx, slaveMeta)

	args = append(args, fmt.Sprintf("-R %v", slaveMeta.ToIPString()))

	for i := range args {
		if args[i] == "<envVars>" {
			newArgs := make([]string, len(args)+len(params)-1)
			copy(newArgs, args[:i])
			copy(newArgs[i:], params)
			copy(newArgs[i+len(params):], args[i+1:])
			args = newArgs
			break
		}
	}

	for i := range args {
		args[i] = os.ExpandEnv(args[i])
	}

	output.PrintLogf("%s Using arguments: %v", ui.IconWorld, args)

	entryPoint := getEntryPoint()
	for i := range execution.Command {
		if execution.Command[i] == "<entryPoint>" {
			execution.Command[i] = entryPoint
		}
	}

	command, args := executor.MergeCommandAndArgs(execution.Command, args)
	// run JMeter inside repo directory ignore execution error in case of failed test
	output.PrintLogf("%s Test run command %s %s", ui.IconRocket, command, strings.Join(args, " "))
	out, err := executor.Run(runPath, command, envManager, args...)
	if err != nil {
		return *result.WithErrors(errors.Errorf("jmeter run error: %v", err)), nil
	}
	out = envManager.ObfuscateSecrets(out)

	output.PrintLogf("%s Getting report %s", ui.IconFile, jtlPath)
	f, err := os.Open(jtlPath)
	if err != nil {
		return *result.WithErrors(errors.Errorf("getting jtl report error: %v", err)), nil
	}

	results, err := parser.ParseCSV(f)
	f.Close()

	var executionResult testkube.ExecutionResult
	if err != nil {
		data, err := os.ReadFile(jtlPath)
		if err != nil {
			return *result.WithErrors(errors.Errorf("getting jtl report error: %v", err)), nil
		}

		testResults, err := parser.ParseXML(data)
		if err != nil {
			return *result.WithErrors(errors.Errorf("parsing jtl report error: %v", err)), nil
		}

		executionResult = mapTestResultsToExecutionResults(out, testResults)
	} else {
		executionResult = mapResultsToExecutionResults(out, results)
	}

	output.PrintLogf("%s Mapped JMeter results to Execution Results...", ui.IconCheckMark)

	if execution.PostRunScript != "" && execution.ExecutePostRunScriptBeforeScraping {
		output.PrintLog(fmt.Sprintf("%s Running post run script...", ui.IconCheckMark))

		if err = agent.RunScript(execution.PostRunScript, r.Params.WorkingDir); err != nil {
			output.PrintLogf("%s Failed to execute post run script %s", ui.IconWarning, err)
		}
	}

	// scrape artifacts first even if there are errors above
	if r.Params.ScrapperEnabled {
		directories := []string{
			outputDir,
		}
		if execution.ArtifactRequest != nil && len(execution.ArtifactRequest.Dirs) != 0 {
			directories = append(directories, execution.ArtifactRequest.Dirs...)
		}

		output.PrintLogf("Scraping directories: %v", directories)
		if err := r.Scraper.Scrape(ctx, directories, execution); err != nil {
			return *executionResult.Err(err), errors.Wrap(err, "error scraping artifacts for JMeter executor")
		}
	}

	return executionResult, nil
}

func getEntryPoint() (entrypoint string) {
	if entrypoint = os.Getenv("ENTRYPOINT_CMD"); entrypoint != "" {
		return entrypoint
	}
	wd, err := os.Getwd()
	if err != nil {
		wd = "."
	}
	return filepath.Join(wd, "scripts/entrypoint.sh")
}

// GetType returns runner type
func (r *JMeterDRunner) GetType() runner.Type {
	return runner.TypeMain
}
