package main

import "log/slog"

const toolArgMaxLen = 512

// logRPCEvent logs a pi RPC event with context-appropriate level and fields.
// Noisy streaming events (message_update, tool_execution_update) are suppressed.
func logRPCEvent(evt rpcEvent) {
	switch evt.Type {
	case rpcTypeMessageUpdate, rpcTypeToolExecutionUpdate:
		// Suppressed — streaming deltas are too noisy.
	case rpcTypeToolExecutionStart:
		slog.Info("pi: tool started", logToolArgs(evt)...)
	case rpcTypeToolExecutionEnd:
		slog.Info("pi: tool finished", "tool", evt.ToolName)
	case rpcTypeAutoRetryStart:
		logAutoRetryStart(evt)
	case rpcTypeAutoRetryEnd:
		logAutoRetryEnd(evt)
	case rpcTypeExtensionError:
		logExtensionError(evt)
	case rpcTypeResponse:
		logResponse(evt)
	default:
		logSimpleRPCEvent(evt)
	}
}

// logSimpleRPCEvent handles events that map directly to a single log line.
func logSimpleRPCEvent(evt rpcEvent) {
	switch evt.Type {
	case rpcTypeAgentStart:
		slog.Info("pi: agent started")
	case rpcTypeAgentEnd:
		slog.Info("pi: agent finished")
	case "compaction_start":
		slog.Info("pi: compaction started", "reason", evt.Reason)
	case "compaction_end":
		slog.Info("pi: compaction finished")
	case "turn_start", "turn_end", "message_start", "message_end", rpcTypeExtensionUIRequest:
		slog.Debug("pi: " + evt.Type)
	default:
		slog.Debug("pi: event", "type", evt.Type)
	}
}

func logAutoRetryStart(evt rpcEvent) {
	slog.Warn("pi: auto-retry",
		"attempt", evt.Attempt,
		"max", evt.MaxAttempts,
		"delay_ms", evt.DelayMs,
		"error", evt.ErrorMessage,
	)
}

func logAutoRetryEnd(evt rpcEvent) {
	if evt.Success != nil && *evt.Success {
		slog.Info("pi: retry succeeded", "attempt", evt.Attempt)
	} else {
		slog.Warn("pi: retries exhausted", "attempt", evt.Attempt, "error", evt.FinalError)
	}
}

func logExtensionError(evt rpcEvent) {
	slog.Error("pi: extension error",
		"extension", evt.ExtensionPath,
		"event", evt.Event,
		"error", evt.Error,
	)
}

func logResponse(evt rpcEvent) {
	ok := evt.Success != nil && *evt.Success
	slog.Debug("pi: response", "command", evt.Command, "success", ok, "error", evt.Error)
}

// logToolArgs returns slog key-value pairs for a tool_execution_start event,
// including the tool name and a summary of the most relevant argument.
func logToolArgs(evt rpcEvent) []any {
	attrs := []any{"tool", evt.ToolName}

	var key string

	switch evt.ToolName {
	case "bash", "Bash":
		key = "command"
	case "Read", "read", "Edit", "edit", "Write", "write":
		key = "path"
	default:
		return attrs
	}

	val, ok := evt.Args[key]
	if !ok {
		return attrs
	}

	s, ok := val.(string)
	if !ok {
		return attrs
	}

	if len(s) > toolArgMaxLen {
		s = s[:toolArgMaxLen] + "…"
	}

	return append(attrs, key, s)
}
