export interface ServerAccess {
  allowed: boolean;
  trial_available: boolean;
  trial_days?: number;
  tier: string;
  trial_end?: string;
  code?: string;
}

export function serverAccessState(access: ServerAccess | null, errorCode?: string) {
  if (errorCode === "subscription_required" || errorCode === "server_subscription_required") return "subscription";
  if (errorCode) return "unavailable";
  if (!access) return "loading";
  if (access.allowed) return "allowed";
  if (access.code === "server_account_required") return "account";
  if (access.trial_available) return "trial";
  return "subscription";
}
