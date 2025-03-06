package plan

import (
	"context"
	"log"
	"plandex-server/db"
	"plandex-server/types"
	"strings"
	"time"

	shared "plandex-shared"
)

var (
	activePlans types.SafeMap[*types.ActivePlan] = *types.NewSafeMap[*types.ActivePlan]()
)

func GetActivePlan(planId, branch string) *types.ActivePlan {
	return activePlans.Get(strings.Join([]string{planId, branch}, "|"))
}

func CreateActivePlan(orgId, userId, planId, branch, prompt string, buildOnly, autoContext bool) *types.ActivePlan {
	activePlan := types.NewActivePlan(orgId, userId, planId, branch, prompt, buildOnly, autoContext)
	key := strings.Join([]string{planId, branch}, "|")

	activePlans.Set(key, activePlan)

	go func() {
		for {
			select {
			case <-activePlan.Ctx.Done():
				log.Printf("case <-activePlan.Ctx.Done(): %s\n", planId)

				err := db.SetPlanStatus(planId, branch, shared.PlanStatusStopped, "")
				if err != nil {
					log.Printf("Error setting plan %s status to stopped: %v\n", planId, err)
				}

				DeleteActivePlan(orgId, userId, planId, branch)

				return
			case apiErr := <-activePlan.StreamDoneCh:
				log.Printf("case apiErr := <-activePlan.StreamDoneCh: %s\n", planId)
				log.Printf("apiErr: %v\n", apiErr)

				if apiErr == nil {
					log.Printf("Plan %s stream completed successfully", planId)

					err := db.SetPlanStatus(planId, branch, shared.PlanStatusFinished, "")
					if err != nil {
						log.Printf("Error setting plan %s status to ready: %v\n", planId, err)
					}

				} else {
					log.Printf("Error streaming plan %s: %v\n", planId, apiErr)

					err := db.SetPlanStatus(planId, branch, shared.PlanStatusError, apiErr.Msg)
					if err != nil {
						log.Printf("Error setting plan %s status to error: %v\n", planId, err)
					}

					log.Println("Sending error message to client")
					activePlan.Stream(shared.StreamMessage{
						Type:  shared.StreamMessageError,
						Error: apiErr,
					})
					activePlan.FlushStreamBuffer()

					log.Println("Stopping any active summary stream")
					activePlan.SummaryCancelFn()

					log.Println("Waiting 100ms after streaming error before canceling active plan")
					time.Sleep(100 * time.Millisecond)
					log.Println("Cancelling active plan")
				}

				activePlan.CancelFn()
				DeleteActivePlan(orgId, userId, planId, branch)
				return
			}
		}
	}()

	return activePlan
}

func DeleteActivePlan(orgId, userId, planId, branch string) {
	log.Printf("Deleting active plan %s - %s - %s\n", planId, branch, orgId)

	activePlan := GetActivePlan(planId, branch)
	if activePlan == nil {
		log.Printf("DeleteActivePlan - No active plan found for plan ID %s on branch %s\n", planId, branch)
		return
	}

	ctx, cancelFn := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelFn()

	err := db.ExecRepoOperation(db.ExecRepoOperationParams{
		OrgId:    orgId,
		UserId:   userId,
		PlanId:   planId,
		Branch:   branch,
		Scope:    db.LockScopeWrite,
		Ctx:      ctx,
		CancelFn: cancelFn,
		Reason:   "delete active plan",
	}, func(repo *db.GitRepo) error {
		return repo.GitClearUncommittedChanges(branch)
	})

	if err != nil {
		log.Printf("Error clearing uncommitted changes for plan %s: %v\n", planId, err)
	}

	activePlans.Delete(strings.Join([]string{planId, branch}, "|"))

	log.Printf("Deleted active plan %s - %s - %s\n", planId, branch, orgId)
}

func UpdateActivePlan(planId, branch string, fn func(*types.ActivePlan)) {
	activePlans.Update(strings.Join([]string{planId, branch}, "|"), fn)
}

func SubscribePlan(ctx context.Context, planId, branch string) (string, chan string) {
	log.Printf("Subscribing to plan %s\n", planId)
	var id string
	var ch chan string

	activePlan := GetActivePlan(planId, branch)
	if activePlan == nil {
		log.Printf("SubscribePlan - No active plan found for plan ID %s on branch %s\n", planId, branch)
		return "", nil
	}

	UpdateActivePlan(planId, branch, func(activePlan *types.ActivePlan) {
		id, ch = activePlan.Subscribe(ctx)
	})
	return id, ch
}

func UnsubscribePlan(planId, branch, subscriptionId string) {
	log.Printf("UnsubscribePlan %s - %s - %s\n", planId, branch, subscriptionId)

	active := GetActivePlan(planId, branch)

	if active == nil {
		log.Printf("No active plan found for plan ID %s on branch %s\n", planId, branch)
		return
	}

	UpdateActivePlan(planId, branch, func(activePlan *types.ActivePlan) {
		activePlan.Unsubscribe(subscriptionId)
		log.Printf("Unsubscribed from plan %s - %s - %s\n", planId, branch, subscriptionId)
	})
}

func NumActivePlans() int {
	return activePlans.Len()
}
