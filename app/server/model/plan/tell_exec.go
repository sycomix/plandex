package plan

import (
	"fmt"
	"log"
	"net/http"
	"time"

	"plandex-server/db"
	"plandex-server/hooks"
	"plandex-server/model"
	"plandex-server/model/prompts"
	"plandex-server/types"

	shared "plandex-shared"

	"github.com/davecgh/go-spew/spew"
	"github.com/google/uuid"
	"github.com/sashabaranov/go-openai"
)

func Tell(clients map[string]model.ClientInfo, plan *db.Plan, branch string, auth *types.ServerAuth, req *shared.TellPlanRequest) error {
	log.Printf("Tell: Called with plan ID %s on branch %s\n", plan.Id, branch)

	_, err := activatePlan(
		clients,
		plan,
		branch,
		auth,
		req.Prompt,
		false,
		req.AutoContext,
	)

	if err != nil {
		log.Printf("Error activating plan: %v\n", err)
		return err
	}

	go execTellPlan(execTellPlanParams{
		clients:            clients,
		plan:               plan,
		branch:             branch,
		auth:               auth,
		req:                req,
		iteration:          0,
		shouldBuildPending: !req.IsChatOnly && req.BuildMode == shared.BuildModeAuto,
	})

	log.Printf("Tell: Tell operation completed successfully for plan ID %s on branch %s\n", plan.Id, branch)
	return nil
}

type execTellPlanParams struct {
	clients                   map[string]model.ClientInfo
	plan                      *db.Plan
	branch                    string
	auth                      *types.ServerAuth
	req                       *shared.TellPlanRequest
	iteration                 int
	missingFileResponse       shared.RespondMissingFileChoice
	shouldBuildPending        bool
	numErrorRetry             int
	shouldLoadFollowUpContext bool
	didLoadFollowUpContext    bool
	didMakeFollowUpPlan       bool
	didLoadChatOnlyContext    bool
}

func execTellPlan(params execTellPlanParams) {
	clients := params.clients
	plan := params.plan
	branch := params.branch
	auth := params.auth
	req := params.req
	iteration := params.iteration
	missingFileResponse := params.missingFileResponse
	shouldBuildPending := params.shouldBuildPending
	shouldLoadFollowUpContext := params.shouldLoadFollowUpContext
	didLoadFollowUpContext := params.didLoadFollowUpContext
	didMakeFollowUpPlan := params.didMakeFollowUpPlan

	log.Printf("[TellExec] Starting iteration %d for plan %s on branch %s", iteration, plan.Id, branch)
	currentUserId := auth.User.Id
	currentOrgId := auth.OrgId

	active := GetActivePlan(plan.Id, branch)

	if active == nil {
		log.Printf("execTellPlan: Active plan not found for plan ID %s on branch %s\n", plan.Id, branch)
		return
	}

	// Load existing subtasks to log their state
	subtasks, err := db.GetPlanSubtasks(currentOrgId, plan.Id)
	if err != nil {
		log.Printf("[TellExec] Error loading subtasks: %v", err)
	} else {
		var unfinished []string
		var finished []string
		for _, task := range subtasks {
			if task.IsFinished {
				finished = append(finished, task.Title)
			} else {
				unfinished = append(unfinished, task.Title)
			}
		}
		log.Printf("[TellExec] Current subtask state - Total: %d, Finished: %d, Unfinished: %d", len(subtasks), len(finished), len(unfinished))
		log.Printf("[TellExec] Finished tasks: %v", finished)
		log.Printf("[TellExec] Unfinished tasks: %v", unfinished)
	}

	if missingFileResponse == "" {
		log.Println("Executing WillExecPlanHook")
		_, apiErr := hooks.ExecHook(hooks.WillExecPlan, hooks.HookParams{
			Auth: auth,
			Plan: plan,
		})

		if apiErr != nil {
			time.Sleep(100 * time.Millisecond)
			active.StreamDoneCh <- apiErr
			return
		}
	}

	planId := plan.Id
	log.Println("execTellPlan - Setting plan status to replying")
	err = db.SetPlanStatus(planId, branch, shared.PlanStatusReplying, "")
	if err != nil {
		log.Printf("Error setting plan %s status to replying: %v\n", planId, err)
		active.StreamDoneCh <- &shared.ApiError{
			Type:   shared.ApiErrorTypeOther,
			Status: http.StatusInternalServerError,
			Msg:    "Error setting plan status to replying",
		}

		log.Printf("execTellPlan: execTellPlan operation completed for plan ID %s on branch %s, iteration %d\n", plan.Id, branch, iteration)
		return
	}
	log.Println("execTellPlan - Plan status set to replying")

	state := &activeTellStreamState{
		execTellPlanParams:  params,
		clients:             clients,
		req:                 req,
		auth:                auth,
		currentOrgId:        currentOrgId,
		currentUserId:       currentUserId,
		plan:                plan,
		branch:              branch,
		iteration:           iteration,
		missingFileResponse: missingFileResponse,
	}

	log.Println("execTellPlan - Loading tell plan")
	err = state.loadTellPlan()
	if err != nil {
		return
	}
	log.Println("execTellPlan - Tell plan loaded")

	log.Printf(`
	iteration: %d
	req.AutoContext: %t,
	req.IsChatOnly: %t,
	req.ExecEnabled: %t,
	req.BuildMode: %s,
	req.IsUserContinue: %t,
	req.IsUserDebug: %t,
	req.IsApplyDebug: %t,
	shouldLoadFollowUpContext: %t,
	didLoadFollowUpContext: %t,
	didMakeFollowUpPlan: %t,
	didLoadChatOnlyContext: %t,
	state.hasContextMap: %t,
	state.hasAssistantReply: %t,
	len(state.subtasks): %d,
	`,
		iteration,
		req.AutoContext,
		req.IsChatOnly,
		req.ExecEnabled,
		req.BuildMode,
		req.IsUserContinue,
		req.IsUserDebug,
		req.IsApplyDebug,
		shouldLoadFollowUpContext,
		didLoadFollowUpContext,
		didMakeFollowUpPlan,
		params.didLoadChatOnlyContext,
		state.hasContextMap,
		state.hasAssistantReply,
		len(state.subtasks),
	)

	var lastConvoMsg *db.ConvoMessage
	if len(state.convo) > 0 {
		lastConvoMsg = state.convo[len(state.convo)-1]
	}

	wasContextStage := lastConvoMsg != nil && lastConvoMsg.Flags.IsContextStage
	didMakePlan := lastConvoMsg != nil && lastConvoMsg.Flags.DidMakePlan
	wasImplementationStage := lastConvoMsg != nil && lastConvoMsg.Flags.IsImplementationStage

	isTrueUserContinue := iteration == 0 && req.IsUserContinue && lastConvoMsg != nil && lastConvoMsg.Role == openai.ChatMessageRoleAssistant
	isUserPrompt := iteration == 0 && (!req.IsChatOnly || !isTrueUserContinue)

	hasSubtasks := len(state.subtasks) > 0

	autoContextEnabled := req.AutoContext && state.hasContextMap
	smartContextEnabled := req.AutoContext

	isFollowUp := iteration == 0 && !isTrueUserContinue && (hasSubtasks || (req.IsChatOnly && state.hasAssistantReply))

	isPlanningStage := req.IsChatOnly ||
		lastConvoMsg == nil ||
		isUserPrompt ||
		(!didMakePlan && !wasImplementationStage)

	isImplementationStage := !req.IsChatOnly && !isPlanningStage

	isContextStage := autoContextEnabled && isPlanningStage && (req.IsChatOnly || !isFollowUp) && !state.contextMapEmpty && !wasContextStage && (isUserPrompt || shouldLoadFollowUpContext)

	log.Printf("isPlanningStage: %t, isImplementationStage: %t, isContextStage: %t, isFollowUp: %t\n", isPlanningStage, isImplementationStage, isContextStage, isFollowUp)

	state.isFollowUp = isFollowUp
	state.willLoadFollowUpContext = shouldLoadFollowUpContext
	state.isPlanningStage = isPlanningStage
	state.isImplementationStage = isImplementationStage
	state.isContextStage = isContextStage

	// if auto context is enabled, we only include maps on the first iteration, which is the context-gathering step, and the second iteration, which is the planning step
	var (
		includeMaps = true
	)
	if req.AutoContext && iteration > 1 {
		includeMaps = false
	}

	modelContextText, err := state.formatModelContext(includeMaps, true, isImplementationStage, smartContextEnabled, req.ExecEnabled)
	if err != nil {
		err = fmt.Errorf("error formatting model modelContext: %v", err)
		log.Println(err)

		active.StreamDoneCh <- &shared.ApiError{
			Type:   shared.ApiErrorTypeOther,
			Status: http.StatusInternalServerError,
			Msg:    "Error formatting model modelContext",
		}
		return
	}

	sysPrompt, err := state.getTellSysPrompt(autoContextEnabled, smartContextEnabled, req.IsChatOnly && params.didLoadChatOnlyContext, modelContextText)
	if err != nil {
		log.Printf("Error getting tell sys prompt: %v\n", err)
		active.StreamDoneCh <- &shared.ApiError{
			Type:   shared.ApiErrorTypeOther,
			Status: http.StatusInternalServerError,
			Msg:    err.Error(),
		}
		return
	}

	// log.Println("**sysPrompt:**\n", sysPrompt)

	state.messages = []openai.ChatCompletionMessage{
		{
			Role:    openai.ChatMessageRoleSystem,
			Content: sysPrompt,
		},
	}

	// Add a separate message for image contexts
	imageContextTokens, ok := state.addImageContext()
	if !ok {
		return
	}

	var applyScriptSummary string
	if req.ExecEnabled {
		if isPlanningStage {
			applyScriptSummary = prompts.ApplyScriptPlanningPromptSummary
		} else {
			applyScriptSummary = prompts.ApplyScriptImplementationPromptSummary
		}
	}

	promptMessage, ok := state.resolvePromptMessage(isPlanningStage, isContextStage, req.IsChatOnly && params.didLoadChatOnlyContext, applyScriptSummary)
	if !ok {
		return
	}

	state.tokensBeforeConvo =
		shared.GetMessagesTokenEstimate(state.messages...) +
			shared.GetMessagesTokenEstimate(*promptMessage) +
			state.latestSummaryTokens +
			imageContextTokens +
			shared.TokensPerRequest

	// print out breakdown of token usage
	log.Printf("Image context tokens: %d\n", imageContextTokens)
	log.Printf("Latest summary tokens: %d\n", state.latestSummaryTokens)
	log.Printf("Total tokens before convo: %d\n", state.tokensBeforeConvo)

	if state.tokensBeforeConvo > state.settings.GetPlannerEffectiveMaxTokens() {
		// token limit already exceeded before adding conversation
		err := fmt.Errorf("token limit exceeded before adding conversation")
		log.Printf("Error: %v\n", err)
		active.StreamDoneCh <- &shared.ApiError{
			Type:   shared.ApiErrorTypeOther,
			Status: http.StatusInternalServerError,
			Msg:    "Token limit exceeded before adding conversation",
		}
		return
	}

	if !state.addConversationMessages() {
		return
	}

	state.replyId = uuid.New().String()
	state.replyParser = types.NewReplyParser()

	if missingFileResponse == "" {
		state.messages = append(state.messages, *promptMessage)
	} else if !state.handleMissingFileResponse(applyScriptSummary) {
		return
	}

	log.Printf("\n\nMessages: %d\n", len(state.messages))
	// for _, message := range state.messages {
	// 	log.Printf("%s: %s\n", message.Role, message.Content)
	// }

	requestTokens := shared.GetMessagesTokenEstimate(state.messages...) + imageContextTokens + shared.TokensPerRequest
	state.totalRequestTokens = requestTokens

	stop := []string{"<PlandexFinish/>"}
	var modelConfig shared.ModelRoleConfig
	if isPlanningStage {
		plannerConfig := state.settings.ModelPack.Planner.GetRoleForTokens(requestTokens)
		modelConfig = plannerConfig.ModelRoleConfig
		if isContextStage {
			log.Println("Tell plan - isContextStage - setting modelConfig to context loader")
			modelConfig = state.settings.ModelPack.GetArchitect().GetRoleForInputTokens(requestTokens)
		}
	} else if isImplementationStage {
		modelConfig = state.settings.ModelPack.GetCoder().GetRoleForInputTokens(requestTokens)
	}

	log.Println("totalRequestTokens:", requestTokens)

	_, apiErr := hooks.ExecHook(hooks.WillSendModelRequest, hooks.HookParams{
		Auth: auth,
		Plan: plan,
		WillSendModelRequestParams: &hooks.WillSendModelRequestParams{
			InputTokens:  requestTokens,
			OutputTokens: modelConfig.GetReservedOutputTokens(),
			ModelName:    modelConfig.BaseModelConfig.ModelName,
		},
	})
	if apiErr != nil {
		active.StreamDoneCh <- apiErr
		return
	}

	// log.Println("Stop:", stop)
	// spew.Dump(state.messages)

	log.Println("modelConfig:", spew.Sdump(modelConfig))

	modelReq := openai.ChatCompletionRequest{
		Model:    modelConfig.BaseModelConfig.ModelName,
		Messages: state.messages,
		Stream:   true,
		StreamOptions: &openai.StreamOptions{
			IncludeUsage: true,
		},
		Temperature: modelConfig.Temperature,
		TopP:        modelConfig.TopP,
		Stop:        stop,
	}

	// output the modelReq to a json file
	// if jsonData, err := json.MarshalIndent(modelReq, "", "  "); err == nil {
	// 	timestamp := time.Now().Format("2006-01-02-150405")
	// 	filename := fmt.Sprintf("generations/model-request-%s.json", timestamp)
	// 	if err := os.WriteFile(filename, jsonData, 0644); err != nil {
	// 		log.Printf("Error writing model request to file: %v\n", err)
	// 	}
	// } else {
	// 	log.Printf("Error marshaling model request to JSON: %v\n", err)
	// }

	stream, err := model.CreateChatCompletionStreamWithRetries(clients, &modelConfig, active.ModelStreamCtx, modelReq)
	if err != nil {
		log.Printf("Error starting reply stream: %v\n", err)

		active.StreamDoneCh <- &shared.ApiError{
			Type:   shared.ApiErrorTypeOther,
			Status: http.StatusInternalServerError,
			Msg:    "Error starting reply stream: " + err.Error(),
		}
		return
	}

	if shouldBuildPending {
		go state.queuePendingBuilds()
	}

	UpdateActivePlan(planId, branch, func(ap *types.ActivePlan) {
		ap.CurrentStreamingReplyId = state.replyId
		ap.CurrentReplyDoneCh = make(chan bool, 1)
	})

	go state.listenStream(stream)
}
