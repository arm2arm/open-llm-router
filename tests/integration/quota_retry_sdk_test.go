package integration_test

import (
	"context"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/arm2arm/open-llm-router/internal/config"
	"github.com/arm2arm/open-llm-router/internal/mockllm"
)

func TestAnthropicDefaultSDKReturnsExhaustedDailyQuotaPromptly(t *testing.T) {
	python := os.Getenv("OLR_TEST_SDK_PYTHON")
	if python == "" {
		t.Skip("set OLR_TEST_SDK_PYTHON to a Python with the official Anthropic SDK; CI also runs this check via make sdk-limits")
	}
	f := startUsageRouter(t, mockllm.New(mockllm.Options{Tokens: 1}), usageOptions{group: &config.Group{Models: []string{"*"}, TokensPerDay: limitPtr(int64(1))}})
	execSQL(t, f.pool, `INSERT INTO usage_daily(day,user_id,model,requests,input_tokens,output_tokens) VALUES(current_date,$1,'chat-model',1,2,1)`, f.owner)
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, python, "../clients/check_quota_retry.py")
	command.Env = append(os.Environ(), "OLR_BASE_URL="+f.router.URL, "OLR_API_KEY="+f.key)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("default Anthropic retry policy: %v\n%s", err, output)
	}
}
