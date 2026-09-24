// errorMessage normalizes an unknown thrown value from a rejected promise.
// ConnectError's message is prefixed with its code ("[not_found] …"), so the
// raw message is preferred for display.
import { ConnectError } from "@connectrpc/connect";

export function errorMessage(err: unknown): string {
  if (err instanceof ConnectError) {
    return err.rawMessage;
  }
  return err instanceof Error ? err.message : String(err);
}
