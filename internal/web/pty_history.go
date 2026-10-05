package web

import (
	"context"
	"errors"
	"sync/atomic"
	"time"

	"tgcontrol/internal/agenthistory"
	"tgcontrol/internal/observability"
)

type historyReader interface {
	ReadAgentHistory(context.Context, string) (agenthistory.Page, error)
}
type historyRequest struct{ ID, Cursor string }
type historyReply struct {
	Type    string             `json:"t"`
	Version int                `json:"v"`
	Request string             `json:"request"`
	Page    *agenthistory.Page `json:"page,omitempty"`
	Error   string             `json:"error,omitempty"`
	// При error=history_version_unsupported — чья и какая версия CLI не
	// поддержана (ST-10 B), чтобы интерфейс назвал её, а не сказал «недоступно».
	// Необязательные поля: старый клиент их не читает (I-14).
	Agent        string `json:"agent,omitempty"`
	AgentVersion string `json:"version,omitempty"`
}

// historyErrorReply раскладывает ошибку чтения в код ответа. Коды — часть
// протокола agent-history v1; прочие ошибки сводятся к history_unavailable,
// чтобы наружу не утекали пути и тексты системных ошибок.
func historyErrorReply(reply historyReply, err error) historyReply {
	reply.Page = nil
	reply.Error = "history_unavailable"
	var version *agenthistory.VersionError
	switch {
	case errors.As(err, &version):
		reply.Error = agenthistory.ErrVersionUnsupported.Error()
		reply.Agent = version.Agent
		reply.AgentVersion = version.Version
	case errors.Is(err, agenthistory.ErrChanged):
		reply.Error = agenthistory.ErrChanged.Error()
	case errors.Is(err, agenthistory.ErrFormat):
		reply.Error = agenthistory.ErrFormat.Error()
	}
	return reply
}

// One bounded worker per viewer; responses join the socket's sole writer.
// The UI explicitly requests pages. Live output never triggers history reads.
type historyChannel struct {
	requests chan historyRequest
	replies  chan historyReply
	busy     atomic.Bool
}

func newHistoryChannel(ctx context.Context, reader historyReader) *historyChannel {
	h := &historyChannel{requests: make(chan historyRequest, 1), replies: make(chan historyReply, 1)}
	go func() {
		defer observability.RecoverPanic("pty-history-read")
		for {
			select {
			case <-ctx.Done():
				return
			case request := <-h.requests:
				readCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
				page, err := reader.ReadAgentHistory(readCtx, request.Cursor)
				cancel()
				reply := historyReply{Type: "agent-history", Version: 1, Request: request.ID, Page: &page}
				if err != nil {
					reply = historyErrorReply(reply, err)
				}
				h.busy.Store(false)
				select {
				case h.replies <- reply:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return h
}

func (h *historyChannel) request(id, cursor string) {
	if id == "" || len(id) > 80 || len(cursor) > 8192 || !h.busy.CompareAndSwap(false, true) {
		return
	}
	h.requests <- historyRequest{ID: id, Cursor: cursor}
}

func terminalCapability(ctrl map[string]any, capability string) bool {
	list, ok := ctrl["capabilities"].([]any)
	if !ok {
		_, present := ctrl["capabilities"]
		return !present && capability == "size-owner-v1"
	} // initial v1 client
	for _, item := range list {
		if item == capability {
			return true
		}
	}
	return false
}
