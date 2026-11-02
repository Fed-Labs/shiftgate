"use client";

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { useEffect, useState, type ReactNode } from "react";
import { registerAuthCacheHooks } from "./store";

export function Providers({ children }: { children: ReactNode }) {
  const [queryClient] = useState(
    () =>
      new QueryClient({
        defaultOptions: {
          queries: {
            staleTime: 30_000,
            retry: 1,
            // Fleet state is operational data: an operator returning to the
            // tab must see current machine/migration state, not a stale
            // snapshot from when the tab was opened.
            refetchOnWindowFocus: true,
          },
        },
      })
  );

  // Auth transitions (logout, session expiry, sign-in as a different user)
  // must drop every cached query: the next user on a shared machine cannot be
  // served the previous tenant's data from this cache. Org switches invalidate
  // instead of clearing, so active views refetch against the new org.
  useEffect(() => {
    registerAuthCacheHooks({
      clear: () => queryClient.clear(),
      invalidateAll: () => {
        void queryClient.invalidateQueries();
      },
    });
    return () => registerAuthCacheHooks(null);
  }, [queryClient]);

  return (
    <QueryClientProvider client={queryClient}>
      {children}
    </QueryClientProvider>
  );
}
