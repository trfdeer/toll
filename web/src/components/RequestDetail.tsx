import { Close } from "@carbon/icons-react";
import {
  IconButton,
  InlineLoading,
  InlineNotification,
  Tag,
} from "@carbon/react";
import type { ChatMessage, RequestDetail as RequestDetailData } from "../lib/types";
import { formatDuration } from "../lib/format";

// tagType maps an HTTP status onto a Carbon tag colour.
function tagType(status: number): "green" | "red" | "magenta" | "gray" {
  if (status >= 200 && status < 300) return "green";
  if (status >= 400 && status < 500) return "red";
  if (status >= 500) return "magenta";
  return "gray";
}

// roleClass whitelists known roles so the class name never contains raw
// upstream text.
function roleClass(role: string): string {
  switch (role) {
    case "system":
    case "developer":
    case "user":
    case "assistant":
    case "tool":
      return role;
    default:
      return "other";
  }
}

// prettyArgs renders tool-call arguments: pretty JSON when they parse, the
// raw string otherwise (some providers stream arguments as plain text).
function prettyArgs(raw: string): string {
  try {
    return JSON.stringify(JSON.parse(raw), null, 2);
  } catch {
    return raw;
  }
}

// noAnswerNote explains an assistant turn that produced no visible content.
function noAnswerNote(
  finishReason: string | undefined,
  hasReasoning: boolean,
): string {
  switch (finishReason) {
    case "length":
    case "max_output_tokens":
      return hasReasoning
        ? "No answer — the model used its whole token budget on thinking."
        : "No answer — the response hit the token limit.";
    case "content_filter":
      return "No answer — the response was filtered.";
    default:
      return finishReason
        ? `No answer returned (${finishReason}).`
        : "No answer returned.";
  }
}

function Message({ message }: { message: ChatMessage }) {
  const role = message.role || "unknown";
  // A responses-format "reasoning" item is just reasoning; render it as a
  // thinking bubble.
  const isThinking = role === "reasoning";
  const thinking = isThinking ? message.content : message.reasoning;
  const hasToolCalls = (message.toolCalls?.length ?? 0) > 0;
  const hasAnswer = message.content !== "" || hasToolCalls;
  const showAnswer = !isThinking && (hasAnswer || message.finishReason !== undefined);
  const roleText = [role, message.name, message.toolCallId]
    .filter(Boolean)
    .join(" · ");

  return (
    <>
      {thinking && (
        <div className="chat-message chat-message--thinking">
          <div className="chat-message__role">thinking</div>
          <div className="chat-message__content">{thinking}</div>
        </div>
      )}
      {showAnswer && (
        <div className={`chat-message chat-message--${roleClass(role)}`}>
          <div className="chat-message__role">
            {roleText}
            {message.finishReason && (
              <span className="chat-message__finish">
                {message.finishReason}
              </span>
            )}
          </div>
          {message.content && (
            <div className="chat-message__content">{message.content}</div>
          )}
          {message.toolCalls?.map((tc, i) => (
            <div className="tool-call" key={i}>
              <div className="tool-call__name">
                {tc.name || "tool"}
                {tc.id && <span className="tool-call__id">{tc.id}</span>}
              </div>
              {tc.arguments && (
                <pre className="tool-call__args">{prettyArgs(tc.arguments)}</pre>
              )}
            </div>
          ))}
          {!hasAnswer && (
            <div className="chat-message__note">
              {noAnswerNote(message.finishReason, Boolean(message.reasoning))}
            </div>
          )}
        </div>
      )}
    </>
  );
}

interface RequestDetailProps {
  detail: RequestDetailData | null;
  loading: boolean;
  error?: string | null;
  onClose: () => void;
}

// RequestDetail is a right-hand drawer showing one request as a chat
// conversation: request turns followed by the assistant reply.
export default function RequestDetail({
  detail,
  loading,
  error,
  onClose,
}: RequestDetailProps) {
  return (
    <>
      <div className="request-detail__scrim" onClick={onClose} />
      <aside className="request-detail" aria-label="Request detail">
        <header className="request-detail__header">
          <div className="request-detail__titles">
            <h2 className="request-detail__title">
              {detail ? detail.gatewayModel : "Request"}
            </h2>
            {detail && (
              <p className="request-detail__meta">
                <Tag type={tagType(detail.status)} size="sm">
                  {detail.status}
                </Tag>
                <span>{detail.createdAt}</span>
                <span>{formatDuration(detail.durationMs)}</span>
                <span>
                  {detail.messages.length} message
                  {detail.messages.length === 1 ? "" : "s"}
                </span>
              </p>
            )}
          </div>
          <IconButton label="Close" kind="ghost" size="sm" onClick={onClose}>
            <Close />
          </IconButton>
        </header>

        <div className="request-detail__body">
          {loading && <InlineLoading description="Loading conversation…" />}
          {!loading && error && (
            <InlineNotification
              kind="error"
              lowContrast
              title="Could not load request"
              subtitle={error}
            />
          )}
          {!loading && !error && detail && !detail.contentStored && (
            <InlineNotification
              kind="info"
              lowContrast
              title="Prompt storage is disabled"
              subtitle="This request's prompt and response were not stored. Turn it on in Settings to inspect new requests."
            />
          )}
          {!loading &&
            !error &&
            detail?.contentStored &&
            detail.messages.length === 0 && (
              <p className="request-detail__empty">
                No conversation recorded for this request.
              </p>
            )}
          {!loading &&
            !error &&
            detail?.messages.map((m, i) => <Message key={i} message={m} />)}
        </div>
      </aside>
    </>
  );
}
