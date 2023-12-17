// Copyright 2022 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package actions

import (
	"bytes"
	git_model "code.gitea.io/gitea/models/git"
	repo_model "code.gitea.io/gitea/models/repo"
	"code.gitea.io/gitea/modules/log"
	"code.gitea.io/gitea/modules/util"
	"fmt"
	"gopkg.in/yaml.v3"
	"net/http"
	"slices"
	"strings"

	actions_model "code.gitea.io/gitea/models/actions"
	"code.gitea.io/gitea/models/db"
	"code.gitea.io/gitea/models/unit"
	"code.gitea.io/gitea/modules/actions"
	"code.gitea.io/gitea/modules/base"
	"code.gitea.io/gitea/modules/container"
	"code.gitea.io/gitea/modules/context"
	"code.gitea.io/gitea/modules/git"
	"code.gitea.io/gitea/modules/setting"
	"code.gitea.io/gitea/routers/web/repo"
	"code.gitea.io/gitea/services/convert"

	"github.com/nektos/act/pkg/model"
)

const (
	tplListActions base.TplName = "repo/actions/list"
	tplViewActions base.TplName = "repo/actions/view"
)

type Workflow struct {
	Entry  git.TreeEntry
	ErrMsg string
}

type WorkflowDispatchInputNode struct {
	Key   string
	Value model.WorkflowDispatchInput
}

// MustEnableActions check if actions are enabled in settings
func MustEnableActions(ctx *context.Context) {
	if !setting.Actions.Enabled {
		ctx.NotFound("MustEnableActions", nil)
		return
	}

	if unit.TypeActions.UnitGlobalDisabled() {
		ctx.NotFound("MustEnableActions", nil)
		return
	}

	if ctx.Repo.Repository != nil {
		if !ctx.Repo.CanRead(unit.TypeActions) {
			ctx.NotFound("MustEnableActions", nil)
			return
		}
	}
}

func List(ctx *context.Context) {
	ctx.Data["Title"] = ctx.Tr("actions.actions")
	ctx.Data["PageIsActions"] = true
	workflowID := ctx.FormString("workflow")
	actorID := ctx.FormInt64("actor")
	status := ctx.FormInt("status")
	ctx.Data["CurWorkflow"] = workflowID

	var workflows []Workflow
	var curWorkflow *model.Workflow
	if empty, err := ctx.Repo.GitRepo.IsEmpty(); err != nil {
		ctx.Error(http.StatusInternalServerError, err.Error())
		return
	} else if !empty {
		commit, err := ctx.Repo.GitRepo.GetBranchCommit(ctx.Repo.Repository.DefaultBranch)
		if err != nil {
			ctx.Error(http.StatusInternalServerError, err.Error())
			return
		}
		entries, err := actions.ListWorkflows(commit)
		if err != nil {
			ctx.Error(http.StatusInternalServerError, err.Error())
			return
		}

		// Get all runner labels
		runners, err := db.Find[actions_model.ActionRunner](ctx, actions_model.FindRunnerOptions{
			RepoID:        ctx.Repo.Repository.ID,
			WithAvailable: true,
		})
		if err != nil {
			ctx.ServerError("FindRunners", err)
			return
		}
		allRunnerLabels := make(container.Set[string])
		for _, r := range runners {
			allRunnerLabels.AddMultiple(r.AgentLabels...)
		}

		workflows = make([]Workflow, 0, len(entries))
		for _, entry := range entries {
			workflow := Workflow{Entry: *entry}
			content, err := actions.GetContentFromEntry(entry)
			if err != nil {
				ctx.Error(http.StatusInternalServerError, err.Error())
				return
			}
			wf, err := model.ReadWorkflow(bytes.NewReader(content))
			if err != nil {
				workflow.ErrMsg = ctx.Locale.Tr("actions.runs.invalid_workflow_helper", err.Error())
				workflows = append(workflows, workflow)
				continue
			}
			// Check whether have matching runner
			for _, j := range wf.Jobs {
				runsOnList := j.RunsOn()
				for _, ro := range runsOnList {
					if strings.Contains(ro, "${{") {
						// Skip if it contains expressions.
						// The expressions could be very complex and could not be evaluated here,
						// so just skip it, it's OK since it's just a tooltip message.
						continue
					}
					if !allRunnerLabels.Contains(ro) {
						workflow.ErrMsg = ctx.Locale.Tr("actions.runs.no_matching_runner_helper", ro)
						break
					}
				}
				if workflow.ErrMsg != "" {
					break
				}
			}
			workflows = append(workflows, workflow)

			if workflow.Entry.Name() == workflowID {
				curWorkflow = wf
			}

		}
	}
	ctx.Data["workflows"] = workflows
	ctx.Data["RepoLink"] = ctx.Repo.Repository.Link()

	page := ctx.FormInt("page")
	if page <= 0 {
		page = 1
	}

	actionsConfig := ctx.Repo.Repository.MustGetUnit(ctx, unit.TypeActions).ActionsConfig()
	ctx.Data["ActionsConfig"] = actionsConfig

	if len(workflowID) > 0 && ctx.Repo.IsAdmin() {
		ctx.Data["AllowDisableOrEnableWorkflow"] = true
		isWorkflowDisabled := actionsConfig.IsWorkflowDisabled(workflowID)
		ctx.Data["CurWorkflowDisabled"] = isWorkflowDisabled

		if !isWorkflowDisabled && curWorkflow != nil {
			workflowDispatchInputs := WorkflowDispatchInputs(curWorkflow.RawOn)
			if workflowDispatchInputs != nil {
				ctx.Data["WorkflowDispatchInputs"] = workflowDispatchInputs

				branchOpts := git_model.FindBranchOptions{
					RepoID:          ctx.Repo.Repository.ID,
					IsDeletedBranch: util.OptionalBoolFalse,
					ListOptions: db.ListOptions{
						ListAll: true,
					},
				}
				branches, err := git_model.FindBranchNames(ctx, branchOpts)
				if err != nil {
					ctx.JSON(http.StatusInternalServerError, err)
					return
				}
				// always put default branch on the top if it exists
				if slices.Contains(branches, ctx.Repo.Repository.DefaultBranch) {
					branches = util.SliceRemoveAll(branches, ctx.Repo.Repository.DefaultBranch)
					branches = append([]string{ctx.Repo.Repository.DefaultBranch}, branches...)
				}
				ctx.Data["Branches"] = branches

				tags, err := repo_model.GetTagNamesByRepoID(ctx, ctx.Repo.Repository.ID)
				if err == nil {
					ctx.Data["Tags"] = tags
				}
			}
		}
	}

	// if status or actor query param is not given to frontend href, (href="/<repoLink>/actions")
	// they will be 0 by default, which indicates get all status or actors
	ctx.Data["CurActor"] = actorID
	ctx.Data["CurStatus"] = status
	if actorID > 0 || status > int(actions_model.StatusUnknown) {
		ctx.Data["IsFiltered"] = true
	}

	opts := actions_model.FindRunOptions{
		ListOptions: db.ListOptions{
			Page:     page,
			PageSize: convert.ToCorrectPageSize(ctx.FormInt("limit")),
		},
		RepoID:        ctx.Repo.Repository.ID,
		WorkflowID:    workflowID,
		TriggerUserID: actorID,
	}

	// if status is not StatusUnknown, it means user has selected a status filter
	if actions_model.Status(status) != actions_model.StatusUnknown {
		opts.Status = []actions_model.Status{actions_model.Status(status)}
	}

	runs, total, err := db.FindAndCount[actions_model.ActionRun](ctx, opts)
	if err != nil {
		ctx.Error(http.StatusInternalServerError, err.Error())
		return
	}

	for _, run := range runs {
		run.Repo = ctx.Repo.Repository
	}

	if err := actions_model.RunList(runs).LoadTriggerUser(ctx); err != nil {
		ctx.Error(http.StatusInternalServerError, err.Error())
		return
	}

	ctx.Data["Runs"] = runs

	actors, err := actions_model.GetActors(ctx, ctx.Repo.Repository.ID)
	if err != nil {
		ctx.Error(http.StatusInternalServerError, err.Error())
		return
	}
	ctx.Data["Actors"] = repo.MakeSelfOnTop(ctx.Doer, actors)

	ctx.Data["StatusInfoList"] = actions_model.GetStatusInfoList(ctx)

	pager := context.NewPagination(int(total), opts.PageSize, opts.Page, 5)
	pager.SetDefaultParams(ctx)
	pager.AddParamString("workflow", workflowID)
	pager.AddParamString("actor", fmt.Sprint(actorID))
	pager.AddParamString("status", fmt.Sprint(status))
	ctx.Data["Page"] = pager

	ctx.HTML(http.StatusOK, tplListActions)
}

func WorkflowDispatchInputs(on yaml.Node) *[]WorkflowDispatchInputNode {
	if on.Kind != yaml.MappingNode {
		return nil
	}
	var val map[string]yaml.Node
	if !decodeNode(on, &val) {
		return nil
	}
	var config map[string]yaml.Node
	if !decodeNode(val["workflow_dispatch"], &config) {
		return nil
	}
	var inputsNode yaml.Node
	if !decodeNode(config["inputs"], &inputsNode) {
		return nil
	}
	var inputs []WorkflowDispatchInputNode
	contentLen := len(inputsNode.Content)
	for i := 0; i < contentLen; i += 2 {
		var input model.WorkflowDispatchInput
		if err := inputsNode.Content[i+1].Decode(&input); err != nil {
			log.Error("Failed to decode dispatch %v into %T: %v", inputsNode, input, err)
			return nil
		}
		inputs = append(inputs, WorkflowDispatchInputNode{
			Key:   inputsNode.Content[i].Value,
			Value: input,
		})
	}
	return &inputs
}

func decodeNode(node yaml.Node, out interface{}) bool {
	if err := node.Decode(out); err != nil {
		log.Error("Failed to decode node %v into %T: %v", node, out, err)
		return false
	}
	return true
}
