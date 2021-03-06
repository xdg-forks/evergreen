package validator

import (
	"bytes"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/evergreen-ci/evergreen"
	"github.com/evergreen-ci/evergreen/command"
	"github.com/evergreen-ci/evergreen/model"
	"github.com/evergreen-ci/evergreen/model/distro"
	"github.com/evergreen-ci/evergreen/util"
	"github.com/evergreen-ci/utility"
	"github.com/mongodb/grip"
	"github.com/mongodb/grip/level"
	"github.com/pkg/errors"
	"gopkg.in/yaml.v2"
)

type projectValidator func(*model.Project) ValidationErrors

type projectSettingsValidator func(*model.Project, *model.ProjectRef) ValidationErrors

type ValidationErrorLevel int64

const (
	Error ValidationErrorLevel = iota
	Warning
	unauthorizedCharacters = "|"
)

func (vel ValidationErrorLevel) String() string {
	switch vel {
	case Error:
		return "ERROR"
	case Warning:
		return "WARNING"
	}
	return "?"
}

type ValidationError struct {
	Level   ValidationErrorLevel `json:"level"`
	Message string               `json:"message"`
}

type ValidationErrors []ValidationError

func (v ValidationErrors) Raw() interface{} {
	return v
}
func (v ValidationErrors) Loggable() bool {
	return len(v) > 0
}
func (v ValidationErrors) String() string {
	out := ""
	for i, validationErr := range v {
		if i > 0 {
			out += "\n"
		}
		out += fmt.Sprintf("%s: %s", validationErr.Level.String(), validationErr.Message)
	}

	return out
}
func (v ValidationErrors) Annotate(key string, value interface{}) error {
	return nil
}
func (v ValidationErrors) Priority() level.Priority {
	return level.Info
}
func (v ValidationErrors) SetPriority(_ level.Priority) error {
	return nil
}

// AtLevel returns all validation errors that match the given level.
func (v ValidationErrors) AtLevel(level ValidationErrorLevel) ValidationErrors {
	errs := ValidationErrors{}
	for _, err := range v {
		if err.Level == level {
			errs = append(errs, err)
		}
	}
	return errs
}

// Functions used to validate the syntax of a project configuration file.
var projectSyntaxValidators = []projectValidator{
	ensureHasNecessaryBVFields,
	checkDependencyGraph,
	validatePluginCommands,
	ensureHasNecessaryProjectFields,
	verifyTaskDependencies,
	verifyTaskRequirements,
	validateTaskNames,
	validateBVNames,
	validateBVBatchTimes,
	validateDisplayTaskNames,
	validateBVTaskNames,
	validateBVsContainTasks,
	checkAllDependenciesSpec,
	validateProjectTaskNames,
	validateProjectTaskIdsAndTags,
	validateTaskGroups,
	validateCreateHosts,
	validateDuplicateBVTasks,
	validateGenerateTasks,
	validateTaskSyncCommands,
}

// Functions used to validate the semantics of a project configuration file.
var projectSemanticValidators = []projectValidator{
	checkTaskCommands,
	checkTaskGroups,
	checkLoggerConfig,
}

var projectSettingsValidators = []projectSettingsValidator{
	validateTaskSyncSettings,
}

func (vr ValidationError) Error() string {
	return vr.Message
}

func ValidationErrorsToString(ves ValidationErrors) string {
	var s bytes.Buffer
	if len(ves) == 0 {
		return s.String()
	}
	for _, ve := range ves {
		s.WriteString(ve.Error())
		s.WriteString("\n")
	}
	return s.String()
}

// getDistros creates a slice of all distro IDs and aliases.
func getDistros() (ids []string, aliases []string, err error) {
	return getDistrosForProject("")
}

// getDistrosForProject creates a slice of all valid distro IDs and a slice of
// all valid aliases for a project. If projectID is empty, it returns all distro
// IDs and all aliases.
func getDistrosForProject(projectID string) (ids []string, aliases []string, err error) {
	// create a slice of all known distros
	distros, err := distro.Find(distro.All)
	if err != nil {
		return nil, nil, err
	}
	for _, d := range distros {
		if projectID != "" && len(d.ValidProjects) > 0 {
			if utility.StringSliceContains(d.ValidProjects, projectID) {
				ids = append(ids, d.Id)
				for _, alias := range d.Aliases {
					if !utility.StringSliceContains(aliases, alias) {
						aliases = append(aliases, alias)
					}
				}
			}
		} else {
			ids = append(ids, d.Id)
			for _, alias := range d.Aliases {
				if !utility.StringSliceContains(aliases, alias) {
					aliases = append(aliases, alias)
				}
			}
		}
	}
	return ids, aliases, nil
}

// verify that the project configuration semantics is valid
func CheckProjectSemantics(project *model.Project) ValidationErrors {
	validationErrs := ValidationErrors{}
	for _, projectSemanticValidator := range projectSemanticValidators {
		validationErrs = append(validationErrs,
			projectSemanticValidator(project)...)
	}
	return validationErrs
}

// verify that the project configuration syntax is valid
func CheckProjectSyntax(project *model.Project) ValidationErrors {
	validationErrs := ValidationErrors{}
	for _, projectSyntaxValidator := range projectSyntaxValidators {
		validationErrs = append(validationErrs,
			projectSyntaxValidator(project)...)
	}

	// get distro IDs and aliases for ensureReferentialIntegrity validation
	distroIDs, distroAliases, err := getDistrosForProject(project.Identifier)
	if err != nil {
		validationErrs = append(validationErrs, ValidationError{Message: "can't get distros from database"})
	}
	validationErrs = append(validationErrs, ensureReferentialIntegrity(project, distroIDs, distroAliases)...)
	return validationErrs
}

// CheckProjectSettings checks the project configuration against the project
// settings.
func CheckProjectSettings(p *model.Project, ref *model.ProjectRef) ValidationErrors {
	var errs ValidationErrors
	for _, validateSettings := range projectSettingsValidators {
		errs = append(errs, validateSettings(p, ref)...)
	}
	return errs
}

func CheckYamlStrict(yamlBytes []byte) ValidationErrors {
	validationErrs := ValidationErrors{}
	// check strict yaml, i.e warn if there are missing fields
	strictProjectWithVariables := struct {
		model.ParserProject `yaml:"pp,inline"`
		// Variables is only used to suppress yaml unmarshalling errors related
		// to a non-existent variables field.
		Variables interface{} `yaml:"variables" bson:"-"`
	}{}

	if err := yaml.UnmarshalStrict(yamlBytes, &strictProjectWithVariables); err != nil {
		validationErrs = append(validationErrs, ValidationError{
			Level:   Warning,
			Message: err.Error(),
		})
	}
	return validationErrs
}

// verify that the project configuration semantics and configuration syntax is valid
func CheckProjectConfigurationIsValid(project *model.Project, pref *model.ProjectRef) error {
	catcher := grip.NewBasicCatcher()
	syntaxErrs := CheckProjectSyntax(project)
	if len(syntaxErrs) != 0 {
		if errs := syntaxErrs.AtLevel(Error); len(errs) != 0 {
			catcher.Errorf("project contains syntax errors: %s", ValidationErrorsToString(errs))
		}
	}
	semanticErrs := CheckProjectSemantics(project)
	if len(semanticErrs) != 0 {
		if errs := semanticErrs.AtLevel(Error); len(errs) != 0 {
			catcher.Errorf("project contains semantic errors: %s", ValidationErrorsToString(errs))
		}
	}
	if settingsErrs := CheckProjectSettings(project, pref); len(settingsErrs) != 0 {
		if errs := settingsErrs.AtLevel(Error); len(errs) != 0 {
			catcher.Errorf("project contains errors related to project settings: %s", ValidationErrorsToString(errs))
		}
	}
	return catcher.Resolve()
}

// ensure that if any task spec references 'model.AllDependencies', it
// references no other dependency within the variant
func checkAllDependenciesSpec(project *model.Project) ValidationErrors {
	errs := ValidationErrors{}
	for _, task := range project.Tasks {
		coveredVariants := map[string]bool{}
		if len(task.DependsOn) > 1 {
			for _, dependency := range task.DependsOn {
				if dependency.Name == model.AllDependencies {
					// incorrect if no variant specified or this variant has already been covered
					if dependency.Variant == "" || coveredVariants[dependency.Variant] {
						errs = append(errs,
							ValidationError{
								Message: fmt.Sprintf("task '%s' in project '%s' "+
									"contains the all dependencies (%s)' "+
									"specification and other explicit dependencies or duplicate variants",
									task.Name, project.Identifier,
									model.AllDependencies),
							},
						)
					}
					coveredVariants[dependency.Variant] = true
				}
			}
		}
	}
	return errs
}

// Makes sure that the dependencies for the tasks in the project form a
// valid dependency graph (no cycles).
func checkDependencyGraph(project *model.Project) ValidationErrors {
	errs := ValidationErrors{}

	tvToTaskUnit := tvToTaskUnit(project)
	visited := map[model.TVPair]bool{}
	allNodes := []model.TVPair{}

	for node := range tvToTaskUnit {
		visited[node] = false
		allNodes = append(allNodes, node)
	}

	for node := range tvToTaskUnit {
		if err := dependencyCycleExists(node, allNodes, visited, tvToTaskUnit); err != nil {
			errs = append(errs, ValidationError{
				Level:   Error,
				Message: fmt.Sprintf("dependency error for '%s' task: %s", node.TaskName, err.Error()),
			})
		}
	}

	return errs
}

// tvToTaskUnit generates all task-variant pairs mapped to their corresponding
// task unit within a build variant.
func tvToTaskUnit(p *model.Project) map[model.TVPair]model.BuildVariantTaskUnit {
	// map of task name and variant -> BuildVariantTaskUnit
	tasksByNameAndVariant := map[model.TVPair]model.BuildVariantTaskUnit{}

	// generate task nodes for every task and variant combination

	taskGroups := map[string]struct{}{}
	for _, tg := range p.TaskGroups {
		taskGroups[tg.Name] = struct{}{}
	}
	for _, bv := range p.BuildVariants {
		tasksToAdd := []model.BuildVariantTaskUnit{}
		for _, t := range bv.Tasks {
			if _, ok := taskGroups[t.Name]; ok {
				tasksToAdd = append(tasksToAdd, model.CreateTasksFromGroup(t, p)...)
			} else {
				tasksToAdd = append(tasksToAdd, t)
			}
		}
		for _, t := range tasksToAdd {
			t.Populate(p.GetSpecForTask(t.Name))
			t.Variant = bv.Name
			node := model.TVPair{
				Variant:  bv.Name,
				TaskName: t.Name,
			}

			tasksByNameAndVariant[node] = t
		}
	}
	return tasksByNameAndVariant
}

// Helper for checking the dependency graph for cycles.
func dependencyCycleExists(node model.TVPair, allNodes []model.TVPair, visited map[model.TVPair]bool,
	tasksByNameAndVariant map[model.TVPair]model.BuildVariantTaskUnit) error {

	v, ok := visited[node]
	// if the node does not exist, the deps are broken
	if !ok {
		return errors.Errorf("dependency %s is not present in the project config", node)
	}
	// if the task has already been visited, then a cycle certainly exists
	if v {
		return errors.Errorf("dependency %s is part of a dependency cycle", node)
	}

	visited[node] = true

	task := tasksByNameAndVariant[node]

	depsToNodes := dependenciesForTaskUnit(node, task, allNodes)
	for _, depNodes := range depsToNodes {
		// For each of the task's dependencies, recursively check for cycles.
		for _, dn := range depNodes {
			if err := dependencyCycleExists(dn, allNodes, visited, tasksByNameAndVariant); err != nil {
				return err
			}
		}
	}

	// remove the task from the visited map so that higher-level calls do not see it
	visited[node] = false

	// no cycle found
	return nil
}

// Ensures that the project has at least one buildvariant and also that all the
// fields required for any buildvariant definition are present
func ensureHasNecessaryBVFields(project *model.Project) ValidationErrors {
	errs := ValidationErrors{}
	if len(project.BuildVariants) == 0 {
		return ValidationErrors{
			{
				Message: fmt.Sprintf("project '%s' must specify at least one "+
					"buildvariant", project.Identifier),
			},
		}
	}

	for _, buildVariant := range project.BuildVariants {
		hasTaskWithoutDistro := false
		if buildVariant.Name == "" {
			errs = append(errs,
				ValidationError{
					Message: fmt.Sprintf("project '%s' buildvariant must "+
						"have a name", project.Identifier),
				},
			)
		}
		if len(buildVariant.Tasks) == 0 {
			errs = append(errs,
				ValidationError{
					Message: fmt.Sprintf("buildvariant '%s' in project '%s' "+
						"must have at least one task", buildVariant.Name,
						project.Identifier),
				},
			)
		}
		for _, task := range buildVariant.Tasks {
			if len(task.Distros) == 0 {
				hasTaskWithoutDistro = true
				break
			}
		}
		if hasTaskWithoutDistro && len(buildVariant.RunOn) == 0 {
			errs = append(errs,
				ValidationError{
					Message: fmt.Sprintf("buildvariant '%s' in project '%s' "+
						"must either specify run_on field or have every task "+
						"specify a distro.",
						buildVariant.Name, project.Identifier),
				},
			)
		}
	}
	return errs
}

// Checks that the basic fields that are required by any project are present and
// valid.
func ensureHasNecessaryProjectFields(project *model.Project) ValidationErrors {
	errs := ValidationErrors{}

	if project.BatchTime < 0 {
		errs = append(errs,
			ValidationError{
				Message: fmt.Sprintf("project '%s' must have a "+
					"non-negative 'batchtime' set", project.Identifier),
			},
		)
	}

	if project.BatchTime > math.MaxInt32 {
		// Error level is warning for backwards compatibility with
		// existing projects. This value will be capped at MaxInt32
		// in ProjectRef.getBatchTime()
		errs = append(errs,
			ValidationError{
				Message: fmt.Sprintf("project '%s' field 'batchtime' should not exceed %d)",
					project.Identifier, math.MaxInt32),
				Level: Warning,
			},
		)
	}

	if project.CommandType != "" {
		if !utility.StringSliceContains(evergreen.ValidCommandTypes, project.CommandType) {
			errs = append(errs,
				ValidationError{
					Message: fmt.Sprintf("project '%s' contains an invalid "+
						"command type: %s", project.Identifier, project.CommandType),
				},
			)
		}
	}
	return errs
}

// Ensures that:
// 1. a referenced task within a buildvariant task object exists in
// the set of project tasks
// 2. any referenced distro exists within the current setting's distro directory
func ensureReferentialIntegrity(project *model.Project, distroIDs []string, distroAliases []string) ValidationErrors {
	errs := ValidationErrors{}
	// create a set of all the task names
	allTaskNames := map[string]bool{}
	for _, task := range project.Tasks {
		allTaskNames[task.Name] = true
	}
	for _, taskGroup := range project.TaskGroups {
		allTaskNames[taskGroup.Name] = true
	}

	for _, buildVariant := range project.BuildVariants {
		buildVariantTasks := map[string]bool{}
		for _, task := range buildVariant.Tasks {
			if _, ok := allTaskNames[task.Name]; !ok {
				if task.Name == "" {
					errs = append(errs,
						ValidationError{
							Message: fmt.Sprintf("tasks for buildvariant '%s' "+
								"in project '%s' must each have a name field",
								project.Identifier, buildVariant.Name),
						},
					)
				} else {
					errs = append(errs,
						ValidationError{
							Message: fmt.Sprintf("buildvariant '%s' in "+
								"project '%s' references a non-existent "+
								"task '%s'", buildVariant.Name,
								project.Identifier, task.Name),
						},
					)
				}
			}
			buildVariantTasks[task.Name] = true
			for _, distro := range task.Distros {
				if !utility.StringSliceContains(distroIDs, distro) && !utility.StringSliceContains(distroAliases, distro) {
					errs = append(errs,
						ValidationError{
							Message: fmt.Sprintf("task '%s' in buildvariant '%s' in project "+
								"'%s' references a nonexistent distro '%s'.\n"+
								task.Name, buildVariant.Name, project.Identifier,
								distro),
							Level: Warning,
						},
					)
				}
			}
		}
		for _, distro := range buildVariant.RunOn {
			if !utility.StringSliceContains(distroIDs, distro) && !utility.StringSliceContains(distroAliases, distro) {
				errs = append(errs,
					ValidationError{
						Message: fmt.Sprintf("buildvariant '%s' in project "+
							"'%s' references a nonexistent distro '%s'.\n"+
							buildVariant.Name,
							project.Identifier, distro),
						Level: Warning,
					},
				)
			}
		}
	}
	return errs
}

// validateTaskNames ensures the task names do not contain unauthorized characters.
func validateTaskNames(project *model.Project) ValidationErrors {
	unauthorizedTaskCharacters := unauthorizedCharacters + " "
	errs := ValidationErrors{}
	for _, task := range project.Tasks {
		if strings.ContainsAny(strings.TrimSpace(task.Name), unauthorizedTaskCharacters) {
			errs = append(errs,
				ValidationError{
					Message: fmt.Sprintf("task name %s contains unauthorized characters ('%s')",
						task.Name, unauthorizedTaskCharacters),
				})
		}
	}
	return errs
}

// Ensures there aren't any duplicate buildvariant names specified in the given
// project and that the names do not contain unauthorized characters.
func validateBVNames(project *model.Project) ValidationErrors {
	errs := ValidationErrors{}
	buildVariantNames := map[string]bool{}
	displayNames := map[string]int{}

	for _, buildVariant := range project.BuildVariants {
		if _, ok := buildVariantNames[buildVariant.Name]; ok {
			errs = append(errs,
				ValidationError{
					Message: fmt.Sprintf("project '%s' buildvariant '%s' already exists",
						project.Identifier, buildVariant.Name),
				},
			)
		}
		buildVariantNames[buildVariant.Name] = true
		dispName := buildVariant.DisplayName
		if dispName == "" { // Default display name to the actual name (identifier)
			dispName = buildVariant.Name
		}
		displayNames[dispName] = displayNames[dispName] + 1

		if strings.ContainsAny(buildVariant.Name, unauthorizedCharacters) {
			errs = append(errs,
				ValidationError{
					Message: fmt.Sprintf("buildvariant name %s contains unauthorized characters (%s)",
						buildVariant.Name, unauthorizedCharacters),
				})
		}
	}
	// don't bother checking for the warnings if we already found errors
	if len(errs) > 0 {
		return errs
	}
	for k, v := range displayNames {
		if v > 1 {
			errs = append(errs,
				ValidationError{
					Level:   Warning,
					Message: fmt.Sprintf("%d build variants share the same display name: '%s'", v, k),
				},
			)

		}
	}
	return errs
}

func checkLoggerConfig(project *model.Project) ValidationErrors {
	errs := ValidationErrors{}
	if project.Loggers != nil {
		if err := project.Loggers.IsValid(); err != nil {
			errs = append(errs, ValidationError{
				Message: errors.Wrap(err, "error in project-level logger config").Error(),
				Level:   Warning,
			})
		}

		for _, task := range project.Tasks {
			for _, command := range task.Commands {
				if err := command.Loggers.IsValid(); err != nil {
					errs = append(errs, ValidationError{
						Message: errors.Wrapf(err, "error in logger config for command %s in task %s", command.DisplayName, task.Name).Error(),
						Level:   Warning,
					})
				}
			}
		}
	}

	return errs
}

// Checks each task definitions to determine if a command is specified
func checkTaskCommands(project *model.Project) ValidationErrors {
	errs := ValidationErrors{}
	for _, task := range project.Tasks {
		if len(task.Commands) == 0 {
			errs = append(errs,
				ValidationError{
					Message: fmt.Sprintf("task '%s' in project '%s' does not "+
						"contain any commands",
						task.Name, project.Identifier),
					Level: Warning,
				},
			)
		}
	}
	return errs
}

// Ensures there aren't any duplicate task names specified for any buildvariant
// in this project
func validateBVTaskNames(project *model.Project) ValidationErrors {
	errs := ValidationErrors{}
	for _, buildVariant := range project.BuildVariants {
		buildVariantTasks := map[string]bool{}
		for _, task := range buildVariant.Tasks {
			if _, ok := buildVariantTasks[task.Name]; ok {
				errs = append(errs,
					ValidationError{
						Message: fmt.Sprintf("task '%s' in buildvariant '%s' "+
							"in project '%s' already exists",
							task.Name, buildVariant.Name, project.Identifier),
					},
				)
			}
			buildVariantTasks[task.Name] = true
		}
	}
	return errs
}

// Ensure there are no buildvariants without tasks
func validateBVsContainTasks(project *model.Project) ValidationErrors {
	errs := ValidationErrors{}
	for _, buildVariant := range project.BuildVariants {
		if len(buildVariant.Tasks) == 0 {
			errs = append(errs,
				ValidationError{
					Message: fmt.Sprintf("buildvariant '%s' contains no tasks", buildVariant.Name),
					Level:   Warning,
				},
			)
		}
	}
	return errs
}

func validateBVBatchTimes(project *model.Project) ValidationErrors {
	errs := ValidationErrors{}
	for _, buildVariant := range project.BuildVariants {
		if buildVariant.CronBatchTime == "" {
			continue
		}
		if buildVariant.BatchTime != nil {
			errs = append(errs,
				ValidationError{
					Message: fmt.Sprintf("variant '%s' cannot specify cron and batchtime", buildVariant.Name),
					Level:   Error,
				})
		}
		if _, err := model.GetActivationTimeWithCron(time.Now(), buildVariant.CronBatchTime); err != nil {
			errs = append(errs,
				ValidationError{
					Message: errors.Wrapf(err, "cron batchtime '%s' has invalid syntax", buildVariant.CronBatchTime).Error(),
					Level:   Error,
				},
			)
		}
	}
	return errs
}

func validateDisplayTaskNames(project *model.Project) ValidationErrors {
	errs := ValidationErrors{}

	// build a map of task names
	tn := map[string]struct{}{}
	for _, t := range project.Tasks {
		tn[t.Name] = struct{}{}
	}

	// check display tasks
	for _, bv := range project.BuildVariants {
		for _, dp := range bv.DisplayTasks {
			for _, etn := range dp.ExecutionTasks {
				if strings.HasPrefix(etn, "display_") {
					errs = append(errs,
						ValidationError{
							Level:   Error,
							Message: fmt.Sprintf("execution task '%s' has prefix 'display_' which is invalid", etn),
						})
				}
			}
		}
	}
	return errs
}

// Helper for validating a set of plugin commands given a project/registry
func validateCommands(section string, project *model.Project,
	commands []model.PluginCommandConf) ValidationErrors {
	errs := ValidationErrors{}

	for _, cmd := range commands {
		commandName := fmt.Sprintf("'%s' command", cmd.Command)
		_, err := command.Render(cmd, project.Functions)
		if err != nil {
			if cmd.Function != "" {
				commandName = fmt.Sprintf("'%s' function", cmd.Function)
			}
			errs = append(errs, ValidationError{Message: fmt.Sprintf("%s section in %s: %s", section, commandName, err)})
		}
		if cmd.Type != "" {
			if !utility.StringSliceContains(evergreen.ValidCommandTypes, cmd.Type) {
				msg := fmt.Sprintf("%s section in '%s': invalid command type: '%s'", section, commandName, cmd.Type)
				errs = append(errs, ValidationError{Message: msg})
			}
		}
	}
	return errs
}

// Ensures there any plugin commands referenced in a project's configuration
// are specified in a valid format
func validatePluginCommands(project *model.Project) ValidationErrors {
	errs := ValidationErrors{}
	seen := make(map[string]bool)

	// validate each function definition
	for funcName, commands := range project.Functions {
		if commands == nil || len(commands.List()) == 0 {
			errs = append(errs,
				ValidationError{
					Message: fmt.Sprintf("'%s' project's '%s' function contains no commands",
						project.Identifier, funcName),
					Level: Error,
				},
			)
			continue
		}
		valErrs := validateCommands("functions", project, commands.List())
		for _, err := range valErrs {
			errs = append(errs,
				ValidationError{
					Message: fmt.Sprintf("'%s' project's '%s' definition: %s",
						project.Identifier, funcName, err),
				},
			)
		}

		for _, c := range commands.List() {
			if c.Function != "" {
				errs = append(errs,
					ValidationError{
						Message: fmt.Sprintf("can not reference a function within a "+
							"function: '%s' referenced within '%s'", c.Function, funcName),
					},
				)

			}
		}

		// this checks for duplicate function definitions in the project.
		if seen[funcName] {
			errs = append(errs,
				ValidationError{
					Message: fmt.Sprintf(`project '%s' has duplicate definition of "%s"`,
						project.Identifier, funcName),
				},
			)
		}
		seen[funcName] = true
	}

	if project.Pre != nil {
		// validate project pre section
		errs = append(errs, validateCommands("pre", project, project.Pre.List())...)
	}

	if project.Post != nil {
		// validate project post section
		errs = append(errs, validateCommands("post", project, project.Post.List())...)
	}

	if project.Timeout != nil {
		// validate project timeout section
		errs = append(errs, validateCommands("timeout", project, project.Timeout.List())...)
	}

	// validate project tasks section
	for _, task := range project.Tasks {
		errs = append(errs, validateCommands("tasks", project, task.Commands)...)
	}
	return errs
}

// Ensures there aren't any duplicate task names for this project
func validateProjectTaskNames(project *model.Project) ValidationErrors {
	errs := ValidationErrors{}
	// create a map to hold the task names
	taskNames := map[string]bool{}
	for _, task := range project.Tasks {
		if _, ok := taskNames[task.Name]; ok {
			errs = append(errs,
				ValidationError{
					Message: fmt.Sprintf("task '%s' in project '%s' "+
						"already exists", task.Name, project.Identifier),
				},
			)
		}
		taskNames[task.Name] = true
	}
	return errs
}

// validateProjectTaskIdsAndTags ensures that task tags and ids only contain valid characters
func validateProjectTaskIdsAndTags(project *model.Project) ValidationErrors {
	errs := ValidationErrors{}
	// create a map to hold the task names
	for _, task := range project.Tasks {
		// check task name
		if i := strings.IndexAny(task.Name, model.InvalidCriterionRunes); i == 0 {
			errs = append(errs, ValidationError{
				Message: fmt.Sprintf("task '%s' has invalid name: starts with invalid character %s",
					task.Name, strconv.QuoteRune(rune(task.Name[0])))})
		}
		// check tag names
		for _, tag := range task.Tags {
			if i := strings.IndexAny(tag, model.InvalidCriterionRunes); i == 0 {
				errs = append(errs, ValidationError{
					Message: fmt.Sprintf("task '%s' has invalid tag '%s': starts with invalid character %s",
						task.Name, tag, strconv.QuoteRune(rune(tag[0])))})
			}
			if i := util.IndexWhiteSpace(tag); i != -1 {
				errs = append(errs, ValidationError{
					Message: fmt.Sprintf("task '%s' has invalid tag '%s': tag contains white space",
						task.Name, tag)})
			}
		}
	}
	return errs
}

// Makes sure that the dependencies for the tasks have the correct fields,
// and that the fields reference valid tasks.
func verifyTaskRequirements(project *model.Project) ValidationErrors {
	errs := ValidationErrors{}
	for _, bvt := range project.FindAllBuildVariantTasks() {
		for _, r := range bvt.Requires {
			if project.FindProjectTask(r.Name) == nil {
				if r.Name == model.AllDependencies {
					errs = append(errs, ValidationError{Message: fmt.Sprintf(
						"task '%s': * is not supported for requirement selectors", bvt.Name)})
				} else {
					errs = append(errs,
						ValidationError{Message: fmt.Sprintf(
							"task '%s' requires non-existent task '%s'", bvt.Name, r.Name)})
				}
			}
			if r.Variant != "" && r.Variant != model.AllVariants && project.FindBuildVariant(r.Variant) == nil {
				errs = append(errs, ValidationError{Message: fmt.Sprintf(
					"task '%s' requires non-existent variant '%s'", bvt.Name, r.Variant)})
			}
			vs := project.FindVariantsWithTask(r.Name)
			if r.Variant != "" && r.Variant != model.AllVariants {
				if !utility.StringSliceContains(vs, r.Variant) {
					errs = append(errs, ValidationError{Message: fmt.Sprintf(
						"task '%s' requires task '%s' on variant '%s'", bvt.Name, r.Name, r.Variant)})
				}
			} else {
				if !utility.StringSliceContains(vs, bvt.Variant) {
					errs = append(errs, ValidationError{Message: fmt.Sprintf(
						"task '%s' requires task '%s' on variant '%s'", bvt.Name, r.Name, bvt.Variant)})
				}
			}
		}
	}
	return errs
}

// Makes sure that the dependencies for the tasks have the correct fields,
// and that the fields have valid values
func verifyTaskDependencies(project *model.Project) ValidationErrors {
	errs := ValidationErrors{}
	// create a set of all the task names
	taskNames := map[string]bool{}
	for _, task := range project.Tasks {
		taskNames[task.Name] = true
	}

	for _, task := range project.Tasks {
		// create a set of the dependencies, to check for duplicates
		depNames := map[model.TVPair]bool{}

		for _, dep := range task.DependsOn {
			// make sure the dependency is not specified more than once
			if depNames[model.TVPair{TaskName: dep.Name, Variant: dep.Variant}] {
				errs = append(errs,
					ValidationError{
						Message: fmt.Sprintf("project '%s' contains a "+
							"duplicate dependency '%s' specified for task '%s'",
							project.Identifier, dep.Name, task.Name),
					},
				)
			}
			depNames[model.TVPair{TaskName: dep.Name, Variant: dep.Variant}] = true

			// check that the status is valid
			switch dep.Status {
			case evergreen.TaskSucceeded, evergreen.TaskFailed, model.AllStatuses, "":
				// these are all valid
			default:
				errs = append(errs,
					ValidationError{
						Message: fmt.Sprintf("project '%s' contains an invalid dependency status for task '%s': %s",
							project.Identifier, task.Name, dep.Status)})
			}

			// check that name of the dependency task is valid
			if dep.Name != model.AllDependencies && !taskNames[dep.Name] {
				errs = append(errs,
					ValidationError{
						Message: fmt.Sprintf("project '%s' contains a "+
							"non-existent task name '%s' in dependencies for "+
							"task '%s'", project.Identifier, dep.Name,
							task.Name),
					},
				)
			}
		}
	}
	return errs
}

func validateTaskGroups(p *model.Project) ValidationErrors {
	errs := ValidationErrors{}

	for _, tg := range p.TaskGroups {
		// validate that there is at least 1 task
		if len(tg.Tasks) < 1 {
			errs = append(errs, ValidationError{
				Message: fmt.Sprintf("task group %s must have at least 1 task", tg.Name),
				Level:   Error,
			})
		}
		// validate that the task group is not named the same as a task
		for _, t := range p.Tasks {
			if t.Name == tg.Name {
				errs = append(errs, ValidationError{
					Message: fmt.Sprintf("%s is used as a name for both a task and task group", t.Name),
					Level:   Error,
				})
			}
		}
		// validate that a task is not listed twice in a task group
		counts := make(map[string]int)
		for _, name := range tg.Tasks {
			counts[name]++
		}
		for name, count := range counts {
			if count > 1 {
				errs = append(errs, ValidationError{
					Message: fmt.Sprintf("%s is listed in task group %s %d times", name, tg.Name, count),
					Level:   Error,
				})
			}
		}
		// validate that attach commands aren't used in the teardown_group phase
		if tg.TeardownGroup != nil {
			for _, cmd := range tg.TeardownGroup.List() {
				if cmd.Command == "attach.results" || cmd.Command == "attach.artifacts" {
					errs = append(errs, ValidationError{
						Message: fmt.Sprintf("%s cannot be used in the group teardown stage", cmd.Command),
						Level:   Error,
					})
				}
			}
		}
	}

	return errs
}

func checkTaskGroups(p *model.Project) ValidationErrors {
	errs := ValidationErrors{}
	tasksInTaskGroups := map[string]string{}
	for _, tg := range p.TaskGroups {
		if tg.MaxHosts < 1 {
			errs = append(errs, ValidationError{
				Message: fmt.Sprintf("task group %s has number of hosts %d less than 1", tg.Name, tg.MaxHosts),
				Level:   Warning,
			})
		}
		if len(tg.Tasks) == 1 {
			continue
		}
		if tg.MaxHosts > (len(tg.Tasks) / 2) {
			errs = append(errs, ValidationError{
				Message: fmt.Sprintf("task group %s has max number of hosts %d greater than half the number of tasks %d", tg.Name, tg.MaxHosts, len(tg.Tasks)),
				Level:   Warning,
			})
		}
		for _, t := range tg.Tasks {
			tasksInTaskGroups[t] = tg.Name
		}
	}
	for t, tg := range tasksInTaskGroups {
		spec := p.GetSpecForTask(t)
		if len(spec.DependsOn) > 0 {
			dependencies := make([]string, 0, len(spec.DependsOn))
			for _, dependsOn := range spec.DependsOn {
				dependencies = append(dependencies, dependsOn.Name)
			}
			errs = append(errs, ValidationError{
				Message: fmt.Sprintf("task %s in task group %s has a dependency on another task (%s), "+
					"which can cause task group tasks to be scheduled out of order", t, tg, dependencies),
				Level: Warning,
			})
		}
	}
	return errs
}

// validateDuplicateBVTasks ensures that no task is used multiple times
// in any given build variant.
func validateDuplicateBVTasks(p *model.Project) ValidationErrors {
	errors := []ValidationError{}

	for _, bv := range p.BuildVariants {
		tasksFound := map[string]interface{}{}
		for _, t := range bv.Tasks {

			if t.IsGroup {
				tg := p.FindTaskGroup(t.Name)
				if tg == nil {
					continue
				}
				for _, tgTask := range tg.Tasks {
					err := checkOrAddTask(tgTask, bv.Name, tasksFound)
					if err != nil {
						errors = append(errors, *err)
					}
				}
			} else {
				err := checkOrAddTask(t.Name, bv.Name, tasksFound)
				if err != nil {
					errors = append(errors, *err)
				}
			}

		}
	}

	return errors
}

func checkOrAddTask(task, variant string, tasksFound map[string]interface{}) *ValidationError {
	if _, found := tasksFound[task]; found {
		return &ValidationError{
			Message: fmt.Sprintf("task '%s' in '%s' is listed more than once, likely through a task group", task, variant),
			Level:   Error,
		}
	}
	tasksFound[task] = nil
	return nil
}

func validateCreateHosts(p *model.Project) ValidationErrors {
	ts := p.TasksThatCallCommand(evergreen.CreateHostCommandName)
	errs := validateTimesCalledPerTask(p, ts, evergreen.CreateHostCommandName, 3, Error)
	errs = append(errs, validateTimesCalledTotal(p, ts, evergreen.CreateHostCommandName, 50)...)
	return errs
}

func validateTimesCalledPerTask(p *model.Project, ts map[string]int, commandName string, times int, level ValidationErrorLevel) (errs ValidationErrors) {
	for _, bv := range p.BuildVariants {
		for _, t := range bv.Tasks {
			if count, ok := ts[t.Name]; ok {
				if count > times {
					errs = append(errs, ValidationError{
						Message: fmt.Sprintf("build variant '%s' with task '%s' may only call %s %d time(s) but calls it %d time(s)", bv.Name, t.Name, commandName, times, count),
						Level:   level,
					})
				}
			}
		}
	}
	return errs
}

func validateTimesCalledTotal(p *model.Project, ts map[string]int, commandName string, times int) (errs ValidationErrors) {
	total := 0
	for _, bv := range p.BuildVariants {
		for _, t := range bv.Tasks {
			if count, ok := ts[t.Name]; ok {
				total += count
			}
		}
	}
	if total > times {
		errs = append(errs, ValidationError{
			Message: fmt.Sprintf("project config may only call %s %d time(s) but it is called %d time(s)", commandName, times, total),
			Level:   Error,
		})
	}
	return errs
}

// validateGenerateTasks validates that no task calls 'generate.tasks' more than once, since if one
// does, the server will noop it.
func validateGenerateTasks(p *model.Project) ValidationErrors {
	ts := p.TasksThatCallCommand(evergreen.GenerateTasksCommandName)
	return validateTimesCalledPerTask(p, ts, evergreen.GenerateTasksCommandName, 1, Error)
}

// validateTaskSyncSettings checks that task sync in the project settings have
// enabled task sync for the config.
func validateTaskSyncSettings(p *model.Project, ref *model.ProjectRef) ValidationErrors {
	if ref.TaskSync.ConfigEnabled {
		return nil
	}
	var errs ValidationErrors
	if s3PushCalls := p.TasksThatCallCommand(evergreen.S3PushCommandName); len(s3PushCalls) != 0 {
		errs = append(errs, ValidationError{
			Level:   Error,
			Message: fmt.Sprintf("cannot use %s command in project config when it is disabled by project settings", evergreen.S3PushCommandName),
		})
	}
	if s3PullCalls := p.TasksThatCallCommand(evergreen.S3PullCommandName); len(s3PullCalls) != 0 {
		errs = append(errs, ValidationError{
			Level:   Error,
			Message: fmt.Sprintf("cannot use %s command in project config when it is disabled by project settings", evergreen.S3PullCommandName),
		})
	}
	return errs
}

// bvsWithTasksThatCallCommand creates a mapping from build variants to tasks
// that run the given command cmd, including the list of matching commands for
// each task.
func bvsWithTasksThatCallCommand(p *model.Project, cmd string) (map[string]map[string][]model.PluginCommandConf, error) {
	// build variant -> tasks that run cmd -> all matching commands
	bvToTasksWithCmds := map[string]map[string][]model.PluginCommandConf{}
	catcher := grip.NewBasicCatcher()

	// addCmdsForTaskInBV adds commands that run for a task in a build variant
	// to the mapping.
	addCmdsForTaskInBV := func(bvToTaskWithCmds map[string]map[string][]model.PluginCommandConf, bv, taskUnit string, cmds []model.PluginCommandConf) {
		if len(cmds) == 0 {
			return
		}
		if _, ok := bvToTaskWithCmds[bv]; !ok {
			bvToTasksWithCmds[bv] = map[string][]model.PluginCommandConf{}
		}
		bvToTasksWithCmds[bv][taskUnit] = append(bvToTasksWithCmds[bv][taskUnit], cmds...)
	}

	for _, bv := range p.BuildVariants {
		var preAndPostCmds []model.PluginCommandConf
		if p.Pre != nil {
			preAndPostCmds = append(preAndPostCmds, p.CommandsRunOnBV(p.Pre.List(), cmd, bv.Name)...)
		}
		if p.Post != nil {
			preAndPostCmds = append(preAndPostCmds, p.CommandsRunOnBV(p.Post.List(), cmd, bv.Name)...)
		}

		for _, bvtu := range bv.Tasks {
			if bvtu.IsGroup {
				tg := p.FindTaskGroup(bvtu.Name)
				if tg == nil {
					catcher.Errorf("cannot find definition of task group '%s' used in build variant '%s'", bvtu.Name, bv.Name)
					continue
				}
				// All setup/teardown commands that apply for this build variant
				// will run for this task.
				var setupAndTeardownCmds []model.PluginCommandConf
				if tg.SetupGroup != nil {
					setupAndTeardownCmds = append(setupAndTeardownCmds, p.CommandsRunOnBV(tg.SetupGroup.List(), cmd, bv.Name)...)
				}
				if tg.SetupTask != nil {
					setupAndTeardownCmds = append(setupAndTeardownCmds, p.CommandsRunOnBV(tg.SetupTask.List(), cmd, bv.Name)...)
				}
				if tg.TeardownGroup != nil {
					setupAndTeardownCmds = append(setupAndTeardownCmds, p.CommandsRunOnBV(tg.TeardownGroup.List(), cmd, bv.Name)...)
				}
				if tg.TeardownTask != nil {
					setupAndTeardownCmds = append(setupAndTeardownCmds, p.CommandsRunOnBV(tg.TeardownTask.List(), cmd, bv.Name)...)
				}
				for _, tgTask := range model.CreateTasksFromGroup(bvtu, p) {
					addCmdsForTaskInBV(bvToTasksWithCmds, bv.Name, tgTask.Name, setupAndTeardownCmds)
					if projTask := p.FindProjectTask(tgTask.Name); projTask != nil {
						cmds := p.CommandsRunOnBV(projTask.Commands, cmd, bv.Name)
						addCmdsForTaskInBV(bvToTasksWithCmds, bv.Name, tgTask.Name, cmds)
					} else {
						catcher.Errorf("cannot find definition of task '%s' used in task group '%s'", tgTask.Name, tg.Name)
					}
				}
			} else {
				// All pre/post commands that apply for this build variant will
				// run for this task.
				addCmdsForTaskInBV(bvToTasksWithCmds, bv.Name, bvtu.Name, preAndPostCmds)

				projTask := p.FindProjectTask(bvtu.Name)
				if projTask == nil {
					catcher.Errorf("cannot find definition of task '%s'", bvtu.Name)
					continue
				}
				cmds := p.CommandsRunOnBV(projTask.Commands, cmd, bv.Name)
				addCmdsForTaskInBV(bvToTasksWithCmds, bv.Name, bvtu.Name, cmds)
			}
		}
	}
	return bvToTasksWithCmds, catcher.Resolve()
}

// validateTaskSyncCommands validates project's task sync commands.  In
// particular, s3.push should be called at most once per task and s3.pull should
// refer to a valid task running s3.push.  It does not check that the project
// settings allow task syncing - see validateTaskSyncSettings.
func validateTaskSyncCommands(p *model.Project) ValidationErrors {
	errs := ValidationErrors{}

	// A task should not call s3.push multiple times.
	s3PushCalls := p.TasksThatCallCommand(evergreen.S3PushCommandName)
	errs = append(errs, validateTimesCalledPerTask(p, s3PushCalls, evergreen.S3PushCommandName, 1, Warning)...)

	bvToTaskCmds, err := bvsWithTasksThatCallCommand(p, evergreen.S3PullCommandName)
	if err != nil {
		errs = append(errs, ValidationError{
			Level:   Error,
			Message: fmt.Sprintf("could not generate map of build variants with tasks that call command '%s': %s", evergreen.S3PullCommandName, err.Error()),
		})
	}

	tvToTaskUnit := tvToTaskUnit(p)
	for bv, taskCmds := range bvToTaskCmds {
		for task, cmds := range taskCmds {
			for _, cmd := range cmds {
				// This is only possible because we disallow expansions for the
				// task and build variant for s3.pull, which would prevent
				// evaluation of dependencies.
				s3PushTaskName, s3PushBVName, parseErr := parseS3PullParameters(cmd)
				if parseErr != nil {
					errs = append(errs, ValidationError{
						Level:   Error,
						Message: fmt.Sprintf("could not parse parameters for command '%s': %s", cmd.Command, parseErr.Error()),
					})
					continue
				}

				// If no build variant is explicitly stated, the build variant
				// is the same as the build variant of the task running s3.pull.
				if s3PushBVName == "" {
					s3PushBVName = bv
				}

				// Since s3.pull depends on the task running s3.push to run
				// first, ensure that this task for this build variant has a
				// dependency on the referenced task and build variant.
				s3PushTaskNode := model.TVPair{TaskName: s3PushTaskName, Variant: s3PushBVName}
				s3PullTaskNode := model.TVPair{TaskName: task, Variant: bv}
				if err := validateTVDependsOnTV(s3PullTaskNode, s3PushTaskNode, tvToTaskUnit); err != nil {
					errs = append(errs, ValidationError{
						Level: Error,
						Message: fmt.Sprintf("problem validating that task running command '%s' depends on task running command '%s': %s",
							evergreen.S3PullCommandName, evergreen.S3PushCommandName, err.Error()),
					})
				}
				// Find the task referenced by s3.pull and ensure that it exists
				// and calls s3.push.
				cmds, err := p.CommandsRunOnTV(s3PushTaskNode, evergreen.S3PushCommandName)
				if err != nil {
					errs = append(errs, ValidationError{
						Level: Error,
						Message: fmt.Sprintf("problem validating that task '%s' runs command '%s': %s",
							s3PushTaskName, evergreen.S3PushCommandName, err.Error()),
					})
				} else if len(cmds) == 0 {
					errs = append(errs, ValidationError{
						Level: Error,
						Message: fmt.Sprintf("task '%s' in build variant '%s' does not run command '%s'",
							s3PushTaskName, s3PushBVName, evergreen.S3PushCommandName),
					})
				}
			}
		}
	}

	return errs
}

// validateTVDependsOnTV checks that the task in the given build variant has a
// dependency on the task in the given build variant.
func validateTVDependsOnTV(source, target model.TVPair, tvToTaskUnit map[model.TVPair]model.BuildVariantTaskUnit) error {
	if source == target {
		return errors.Errorf("task '%s' in build variant '%s' cannot depend on itself",
			source.TaskName, source.Variant)
	}
	visited := map[model.TVPair]bool{}
	var allTVs []model.TVPair
	for tv := range tvToTaskUnit {
		visited[tv] = false
		allTVs = append(allTVs, tv)
	}

	sourceTask, ok := tvToTaskUnit[source]
	if !ok {
		return errors.Errorf("could not find task '%s' in build variant '%s'",
			source.TaskName, source.Variant)
	}

	// patches and mainline builds shouldn't depend on anything that's git tag only,
	// while something that could run in a git tag build can't depend on something that's patchOnly.
	// requireOnNonGitTag is just requireOnPatches & requireOnNonPatches so we don't consider this case.
	depReqs := dependencyRequirements{
		lastDepNeedsSuccess: true,
		requireOnPatches:    !sourceTask.SkipOnPatchBuild() && !sourceTask.SkipOnNonGitTagBuild(),
		requireOnNonPatches: !sourceTask.SkipOnNonPatchBuild() && !sourceTask.SkipOnNonGitTagBuild(),
		requireOnGitTag:     !sourceTask.SkipOnNonPatchBuild(),
	}
	depFound, err := dependencyMustRun(target, source, depReqs, allTVs, visited, tvToTaskUnit)
	if err != nil {
		return errors.Wrapf(err, "error searching for dependency of task '%s' in build variant '%s'"+
			" on task '%s' in build variant '%s'",
			source.TaskName, source.Variant,
			target.TaskName, target.Variant)
	}
	if !depFound {
		errMsg := "task '%s' on build variant '%s' must depend on" +
			" task '%s' in build variant '%s' running and succeeding"
		if depReqs.requireOnPatches && depReqs.requireOnNonPatches {
			errMsg += " for both patches and non-patches"
		} else if depReqs.requireOnPatches {
			errMsg += " for patches"
		} else if depReqs.requireOnNonPatches {
			errMsg += " for non-patches"
		} else if depReqs.requireOnGitTag {
			errMsg += " for git-tag builds"
		}
		return errors.Errorf(errMsg, source.TaskName, source.Variant, target.TaskName, target.Variant)
	}
	return nil
}

type dependencyRequirements struct {
	lastDepNeedsSuccess bool
	requireOnPatches    bool
	requireOnNonPatches bool
	requireOnGitTag     bool
}

// dependencyMustRun checks whether or not the current task in a build
// variant depends on the success of the target task in the build variant.
func dependencyMustRun(target model.TVPair, current model.TVPair, depReqs dependencyRequirements, allNodes []model.TVPair, visited map[model.TVPair]bool, tvToTaskUnit map[model.TVPair]model.BuildVariantTaskUnit) (bool, error) {
	isVisited, ok := visited[current]
	// If the node is missing, the dependency graph is malformed.
	if !ok {
		return false, errors.Errorf("dependency '%s' in variant '%s' is not defined", current.TaskName, current.Variant)
	}
	// If a node is revisited on this DFS, the dependency graph cannot be
	// checked because it has a cycle.
	if isVisited {
		return false, errors.Errorf("dependency '%s' in variant '%s' is in a dependency cycle", current.TaskName, current.Variant)
	}

	taskUnit := tvToTaskUnit[current]
	// Even if current depends on target according to the dependency graph, if
	// the current task will not run in the same cases as the source (e.g. the
	// source task runs on patches but current task does not, or if the current task
	// is only available to git tag builds), the dependency is
	// not reachable from this branch.
	if depReqs.requireOnPatches && (taskUnit.SkipOnPatchBuild() || taskUnit.SkipOnNonGitTagBuild()) {
		return false, nil
	}
	if depReqs.requireOnNonPatches && (taskUnit.SkipOnNonPatchBuild() || taskUnit.SkipOnNonGitTagBuild()) {
		return false, nil
	}
	if depReqs.requireOnGitTag && (taskUnit.SkipOnNonPatchBuild()) {
		return false, nil
	}

	if current == target {
		return depReqs.lastDepNeedsSuccess, nil
	}

	visited[current] = true

	depsToNodes := dependenciesForTaskUnit(current, taskUnit, allNodes)
	for dep, depNodes := range depsToNodes {
		// If the task must run on patches but this dependency is optional on
		// patches, we cannot traverse this dependency branch.
		if depReqs.requireOnPatches && dep.PatchOptional {
			continue
		}
		depReqs.lastDepNeedsSuccess = dep.Status == "" || dep.Status == evergreen.TaskSucceeded

		for _, depNode := range depNodes {
			reachable, err := dependencyMustRun(target, depNode, depReqs, allNodes, visited, tvToTaskUnit)
			if err != nil {
				return false, errors.Wrap(err, "dependency graph has problems")
			}
			if reachable {
				return true, nil
			}
		}
	}

	visited[current] = false

	return false, nil
}

// dependenciesForTaskUnit returns a map of this task unit's dependencies to
// and all the task-build variant pairs the task unit depends on.
func dependenciesForTaskUnit(tv model.TVPair, taskUnit model.BuildVariantTaskUnit, allTVs []model.TVPair) map[model.TaskUnitDependency][]model.TVPair {
	depsToNodes := map[model.TaskUnitDependency][]model.TVPair{}
	for _, dep := range taskUnit.DependsOn {
		if dep.Variant != model.AllVariants {
			// Handle dependencies with one variant.

			depTV := model.TVPair{TaskName: dep.Name, Variant: dep.Variant}
			if depTV.Variant == "" {
				// Use the current variant if none is specified.
				depTV.Variant = tv.Variant
			}

			if depTV.TaskName == model.AllDependencies {
				// Handle dependencies with all-dependencies by adding all the
				// variant's tasks except the current one.
				for _, currTV := range allTVs {
					if currTV.TaskName != tv.TaskName && currTV.Variant == depTV.Variant {
						depsToNodes[dep] = append(depsToNodes[dep], currTV)
					}
				}
			} else {
				// Normal case: just append the dependency with its task and
				// variant.
				depsToNodes[dep] = append(depsToNodes[dep], depTV)
			}
		} else {
			// Handle dependencies with all-variants.

			if dep.Name != model.AllDependencies {
				// Handle dependencies with all-variants by adding the task from
				// all variants except the current task-variant.
				for _, currTV := range allTVs {
					if currTV.TaskName == dep.Name && (currTV != tv) {
						depsToNodes[dep] = append(depsToNodes[dep], currTV)
					}
				}
			} else {
				// Handle dependencies with all-variants and all-dependencies by
				// adding all the tasks except the current one.
				for _, currTV := range allTVs {
					if currTV != tv {
						depsToNodes[dep] = append(depsToNodes[dep], currTV)
					}
				}
			}
		}
	}

	return depsToNodes
}

// parseS3PullParameters returns the parameters from the s3.pull command that
// references the push task.
func parseS3PullParameters(c model.PluginCommandConf) (task, bv string, err error) {
	if len(c.Params) == 0 {
		return "", "", errors.Errorf("command '%s' has no parameters", c.Command)
	}
	var i interface{}
	var ok bool
	var paramName string

	paramName = "task"
	i, ok = c.Params[paramName]
	if !ok {
		return "", "", errors.Errorf("command '%s' needs parameter '%s' defined", c.Command, paramName)
	} else {
		task, ok = i.(string)
		if !ok {
			return "", "", errors.Errorf("command '%s' was supplied parameter '%s' but is not a string argument, got %T", c.Command, paramName, i)
		}
	}

	paramName = "from_build_variant"
	i, ok = c.Params[paramName]
	if !ok {
		return task, "", nil
	}
	bv, ok = i.(string)
	if !ok {
		return "", "", errors.Errorf("command '%s' was supplied parameter '%s' but is not a string argument, got %T", c.Command, paramName, i)
	}
	return task, bv, nil
}
