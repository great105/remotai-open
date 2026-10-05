package hermes

import (
	"encoding/json"
	"errors"
)

// Authorize the exact native request before the durable single-use boundary.
func validateApprovalReply(record *AttentionRecord, raw json.RawMessage) error {
	if record.Kind == "question" {
		var params struct {
			Questions []struct {
				QID string `json:"qid"`
			} `json:"questions"`
		}
		var result struct {
			Answers map[string]*string `json:"answers"`
		}
		if json.Unmarshal(record.Params, &params) != nil || len(params.Questions) == 0 || len(params.Questions) > 5 || DecodeControlRequest(raw, &result) != nil {
			return errors.New("некорректный ответ на вопрос Hermes")
		}
		known := map[string]bool{}
		for _, q := range params.Questions {
			known[q.QID] = true
		}
		for qid, answer := range result.Answers {
			if !known[qid] || answer != nil && len(*answer) > 16<<10 {
				return errors.New("ответ не соответствует вопросам Hermes")
			}
		}
		// Native multi-select and custom answers are strings, not JSON arrays;
		// null is a skipped question and absent answers cancels the request.
		return nil
	}
	if record.Kind != "approval" {
		switch record.Method {
		case "secret", "sudo", "vault.unlock_prompt", "vault.save_login", "vault.code":
			var result struct {
				Value *string `json:"value"`
			}
			if DecodeControlRequest(raw, &result) != nil || result.Value == nil || len(*result.Value) > 32<<10 {
				return errors.New("некорректное приватное значение Hermes")
			}
			return nil
		default:
			return errors.New("неподдерживаемый запрос Hermes")
		}
	}
	var params struct {
		Choices        []string `json:"choices"`
		AllowSession   *bool    `json:"allow_session"`
		AllowPermanent *bool    `json:"allow_permanent"`
		SmartDenied    bool     `json:"smart_denied"`
	}
	var result struct {
		Choice string `json:"choice"`
		All    *bool  `json:"all"`
	}
	if json.Unmarshal(record.Params, &params) != nil || DecodeControlRequest(raw, &result) != nil {
		return errors.New("некорректное разрешение Hermes")
	}
	allowed := false
	for _, choice := range params.Choices {
		if choice == result.Choice {
			allowed = true
		}
	}
	if result.Choice != "once" && result.Choice != "deny" && result.Choice != "session" && result.Choice != "always" {
		allowed = false
	}
	if (result.Choice == "session" || result.Choice == "always") && (params.SmartDenied || params.AllowSession != nil && !*params.AllowSession) {
		allowed = false
	}
	if result.Choice == "always" && params.AllowPermanent != nil && !*params.AllowPermanent {
		allowed = false
	}
	// Bulk approval can authorize other requests with different contextual gates.
	if result.All != nil && *result.All {
		allowed = false
	}
	if !allowed {
		return errors.New("ответ не разрешён этим запросом Hermes")
	}
	return nil
}
