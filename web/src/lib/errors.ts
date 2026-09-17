// errorMessage normalizes an unknown thrown value from a rejected promise.
export function errorMessage(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}
