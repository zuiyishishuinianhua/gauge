package main

import (
	"errors"
	"fmt"
	"github.com/getgauge/common"
	"github.com/getgauge/gauge/config"
	"github.com/getgauge/gauge/gauge_messages"
	"github.com/golang/protobuf/proto"
	"strings"
)

type rephraseRefactorer struct {
	oldStep   *step
	newStep   *step
	isConcept bool
}

type refactoringResult struct {
	success            bool
	specsChanged       []string
	conceptsChanged    []string
	runnerFilesChanged []string
	errors             []string
	warnings           []string
}

func performRephraseRefactoring(oldStep, newStep string) *refactoringResult {
	if newStep == oldStep {
		return rephraseFailure("Same old step name and new step name.")
	}
	agent, err := getRefactorAgent(oldStep, newStep)

	if err != nil {
		return rephraseFailure(err.Error())
	}

	projectRoot, err := common.GetProjectRoot()
	if err != nil {
		return rephraseFailure(err.Error())
	}

	result := &refactoringResult{success: true, errors: make([]string, 0), warnings: make([]string, 0)}
	specs, specParseResults := findSpecs(projectRoot, &conceptDictionary{})
	addErrorsAndWarningsToRefactoringResult(result, specParseResults...)
	if !result.success {
		return result
	}
	conceptDictionary, parseResult := createConceptsDictionary(false)

	addErrorsAndWarningsToRefactoringResult(result, parseResult)
	if !result.success {
		return result
	}

	refactorResult := agent.performRefactoringOn(specs, conceptDictionary)
	refactorResult.warnings = append(refactorResult.warnings, result.warnings...)
	return refactorResult
}

func rephraseFailure(errors ...string) *refactoringResult {
	return &refactoringResult{success: false, errors: errors}
}

func addErrorsAndWarningsToRefactoringResult(refactorResult *refactoringResult, parseResults ...*parseResult) {
	for _, parseResult := range parseResults {
		if !parseResult.ok {
			refactorResult.success = false
			refactorResult.errors = append(refactorResult.errors, parseResult.error.Error())
		}
		refactorResult.appendWarnings(parseResult.warnings)
	}
}

func (agent *rephraseRefactorer) performRefactoringOn(specs []*specification, conceptDictionary *conceptDictionary) *refactoringResult {
	specsRefactored, conceptFilesRefactored := agent.rephraseInSpecsAndConcepts(&specs, conceptDictionary)
	specFiles, conceptFiles := writeToConceptAndSpecFiles(specs, conceptDictionary, specsRefactored, conceptFilesRefactored)
	refactoringResult := &refactoringResult{specsChanged: specFiles, success: false, conceptsChanged: conceptFiles, errors: make([]string, 0)}

	runner, connErr := agent.startRunner()
	if connErr != nil {
		refactoringResult.errors = append(refactoringResult.errors, connErr.Error())
		return refactoringResult
	}
	defer runner.kill()
	stepName, err := agent.getStepNameFromRunner(runner)
	if err != nil {
		refactoringResult.errors = append(refactoringResult.errors, err.Error())
		return refactoringResult
	}
	runnerFilesChanged, err := agent.requestRunnerForRefactoring(runner, stepName)
	if err != nil {
		refactoringResult.errors = append(refactoringResult.errors, fmt.Sprintf("Only spec files and concepts refactored: %s", err))
		return refactoringResult
	}
	refactoringResult.success = true
	refactoringResult.runnerFilesChanged = runnerFilesChanged
	return refactoringResult
}

func (agent *rephraseRefactorer) rephraseInSpecsAndConcepts(specs *[]*specification, conceptDictionary *conceptDictionary) (map[*specification]bool, map[string]bool) {
	specsRefactored := make(map[*specification]bool, 0)
	conceptFilesRefactored := make(map[string]bool, 0)
	orderMap := agent.createOrderOfArgs()
	for _, spec := range *specs {
		specsRefactored[spec] = spec.renameSteps(*agent.oldStep, *agent.newStep, orderMap)
	}
	isConcept := false
	for _, concept := range conceptDictionary.conceptsMap {
		_, ok := conceptFilesRefactored[concept.fileName]
		conceptFilesRefactored[concept.fileName] = !ok && false || conceptFilesRefactored[concept.fileName]
		for _, item := range concept.conceptStep.items {
			isRefactored := conceptFilesRefactored[concept.fileName]
			conceptFilesRefactored[concept.fileName] = item.kind() == stepKind &&
				item.(*step).rename(*agent.oldStep, *agent.newStep, isRefactored, orderMap, &isConcept) ||
				isRefactored
		}
	}
	agent.isConcept = isConcept
	return specsRefactored, conceptFilesRefactored
}

func (agent *rephraseRefactorer) createOrderOfArgs() map[int]int {
	orderMap := make(map[int]int, len(agent.newStep.args))
	for i, arg := range agent.newStep.args {
		orderMap[i] = SliceIndex(len(agent.oldStep.args), func(i int) bool { return agent.oldStep.args[i].String() == arg.String() })
	}
	return orderMap
}

func SliceIndex(limit int, predicate func(i int) bool) int {
	for i := 0; i < limit; i++ {
		if predicate(i) {
			return i
		}
	}
	return -1
}

func getRefactorAgent(oldStepText, newStepText string) (*rephraseRefactorer, error) {
	parser := new(specParser)
	stepTokens, err := parser.generateTokens("* " + oldStepText + "\n" + "*" + newStepText)
	if err != nil {
		return nil, err
	}
	spec := &specification{}
	steps := make([]*step, 0)
	for _, stepToken := range stepTokens {
		step, err := spec.createStepUsingLookup(stepToken, nil)
		if err != nil {
			return nil, err
		}
		steps = append(steps, step)
	}
	return &rephraseRefactorer{oldStep: steps[0], newStep: steps[1]}, nil
}

func (agent *rephraseRefactorer) requestRunnerForRefactoring(testRunner *testRunner, stepName string) ([]string, error) {
	refactorRequest, err := agent.createRefactorRequest(testRunner, stepName)
	if err != nil {
		return nil, err
	}
	refactorResponse := agent.sendRefactorRequest(testRunner, refactorRequest)
	var runnerError error
	if !refactorResponse.GetSuccess() {
		apiLog.Error("Refactoring error response from runner: %v", refactorResponse.GetError())
		runnerError = errors.New(refactorResponse.GetError())
	}
	return refactorResponse.GetFilesChanged(), runnerError
}

func (agent *rephraseRefactorer) startRunner() (*testRunner, error) {
	loadGaugeEnvironment()
	startAPIService(0)
	testRunner, err := startRunnerAndMakeConnection(getProjectManifest())
	if err != nil {
		return nil, errors.New(fmt.Sprintf("Failed to connect to test runner: %s", err))
	}
	return testRunner, nil
}

func (agent *rephraseRefactorer) sendRefactorRequest(testRunner *testRunner, refactorRequest *gauge_messages.Message) *gauge_messages.RefactorResponse {
	response, err := getResponseForMessageWithTimeout(refactorRequest, testRunner.connection, config.RefactorTimeout())
	if err != nil {
		return &gauge_messages.RefactorResponse{Success: proto.Bool(false), Error: proto.String(err.Error())}
	}
	return response.GetRefactorResponse()
}

//Todo: Check for inline tables
func (agent *rephraseRefactorer) createRefactorRequest(runner *testRunner, stepName string) (*gauge_messages.Message, error) {
	oldStepValue, err := agent.getStepValueFor(agent.oldStep, stepName)
	if err != nil {
		return nil, err
	}
	orderMap := agent.createOrderOfArgs()
	newStepName := agent.generateNewStepName(oldStepValue.args, orderMap)
	newStepValue, err := extractStepValueAndParams(newStepName, false)
	if err != nil {
		return nil, err
	}
	oldProtoStepValue := convertToProtoStepValue(oldStepValue)
	newProtoStepValue := convertToProtoStepValue(newStepValue)
	return &gauge_messages.Message{MessageType: gauge_messages.Message_RefactorRequest.Enum(), RefactorRequest: &gauge_messages.RefactorRequest{OldStepValue: oldProtoStepValue, NewStepValue: newProtoStepValue, ParamPositions: agent.createParameterPositions(orderMap)}}, nil
}

func (agent *rephraseRefactorer) generateNewStepName(args []string, orderMap map[int]int) string {
	agent.newStep.populateFragments()
	paramIndex := 0
	for _, fragment := range agent.newStep.fragments {
		if fragment.GetFragmentType() == gauge_messages.Fragment_Parameter {
			if orderMap[paramIndex] != -1 {
				fragment.GetParameter().Value = proto.String(args[orderMap[paramIndex]])
			}
			paramIndex++
		}
	}
	return convertToStepText(agent.newStep.fragments)
}

func (agent *rephraseRefactorer) getStepNameFromRunner(runner *testRunner) (string, error) {
	stepNameMessage := &gauge_messages.Message{MessageType: gauge_messages.Message_StepNameRequest.Enum(), StepNameRequest: &gauge_messages.StepNameRequest{StepValue: proto.String(agent.oldStep.value)}}
	responseMessage, err := getResponseForMessageWithTimeout(stepNameMessage, runner.connection, config.RunnerAPIRequestTimeout())
	if err != nil {
		return "", err
	}
	if !(responseMessage.GetStepNameResponse().GetIsStepPresent()) {
		return "", errors.New(fmt.Sprintf("Step implementation not found: %s", agent.oldStep.lineText))
	}
	if responseMessage.GetStepNameResponse().GetHasAlias() {
		return "", errors.New(fmt.Sprintf("steps with aliases : '%s' cannot be refactored.", strings.Join(responseMessage.GetStepNameResponse().GetStepName(), "', '")))
	}
	return responseMessage.GetStepNameResponse().GetStepName()[0], nil
}

func (agent *rephraseRefactorer) createParameterPositions(orderMap map[int]int) []*gauge_messages.ParameterPosition {
	paramPositions := make([]*gauge_messages.ParameterPosition, 0)
	for k, v := range orderMap {
		paramPositions = append(paramPositions, &gauge_messages.ParameterPosition{NewPosition: proto.Int(k), OldPosition: proto.Int(v)})
	}
	return paramPositions
}

func (agent *rephraseRefactorer) getStepValueFor(step *step, stepName string) (*stepValue, error) {
	return extractStepValueAndParams(stepName, false)
}

func writeToConceptAndSpecFiles(specs []*specification, conceptDictionary *conceptDictionary, specsRefactored map[*specification]bool, conceptFilesRefactored map[string]bool) ([]string, []string) {
	specFiles := make([]string, 0)
	conceptFiles := make([]string, 0)
	for _, spec := range specs {
		if specsRefactored[spec] {
			specFiles = append(specFiles, spec.fileName)
			formatted := formatSpecification(spec)
			saveFile(spec.fileName, formatted, true)
		}
	}
	conceptMap := formatConcepts(conceptDictionary)
	for fileName, concept := range conceptMap {
		if conceptFilesRefactored[fileName] {
			conceptFiles = append(conceptFiles, fileName)
			saveFile(fileName, concept, true)
		}
	}
	return specFiles, conceptFiles
}

func (refactoringResult *refactoringResult) appendWarnings(warnings []*warning) {
	if refactoringResult.warnings == nil {
		refactoringResult.warnings = make([]string, 0)
	}
	for _, warning := range warnings {
		refactoringResult.warnings = append(refactoringResult.warnings, warning.message)
	}
}

func (refactoringResult *refactoringResult) allFilesChanges() []string {
	filesChanged := make([]string, 0)
	filesChanged = append(filesChanged, refactoringResult.specsChanged...)
	filesChanged = append(filesChanged, refactoringResult.conceptsChanged...)
	filesChanged = append(filesChanged, refactoringResult.runnerFilesChanged...)
	return filesChanged

}
