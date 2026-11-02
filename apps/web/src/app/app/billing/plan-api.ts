import { api } from "@/lib/api";
import type {
  TrialStatus,
  TrialStarted,
  UsageSummaryResponse,
  CheckoutOptions,
} from "@/lib/types";

// Billing/trial calls the dashboard's billing, team, and storage pages need
// but the shared ApiClient does not expose yet. The transport is the client's
// own request seam, so auth headers, session refresh, and the ApiError shape
// stay identical to every other call; when lib/api.ts grows first-class
// methods for these routes, each function below delegates unchanged.
type ApiTransport = {
  request<T>(path: string, init?: RequestInit, retry?: boolean): Promise<T>;
};

const transport = api as unknown as ApiTransport;

export function getTrialStatus(orgId: string): Promise<TrialStatus> {
  return transport.request<TrialStatus>(`/v1/organizations/${orgId}/trial`);
}

export function startTrial(orgId: string): Promise<TrialStarted> {
  return transport.request<TrialStarted>(`/v1/organizations/${orgId}/trial`, {
    method: "POST",
    body: JSON.stringify({}),
  });
}

export function getUsageSummary(orgId: string): Promise<UsageSummaryResponse> {
  return transport.request<UsageSummaryResponse>(
    `/v1/organizations/${orgId}/usage/summary`
  );
}

export function startCheckoutSession(
  orgId: string,
  body: CheckoutOptions
): Promise<{ url: string; session_id: string; plan: string }> {
  return transport.request<{ url: string; session_id: string; plan: string }>(
    `/v1/organizations/${orgId}/billing/checkout`,
    { method: "POST", body: JSON.stringify(body) }
  );
}
