// App — the screen switcher. Opening a migration from Workloads or History
// swaps the main view to the live migration pipeline; closing it returns to
// the previous screen.

import { useEffect, useState } from "react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { Shell, type Screen } from "@/components/shell";
import { DashboardScreen } from "@/screens/Dashboard";
import { MachinesScreen } from "@/screens/Machines";
import { WorkloadsScreen } from "@/screens/Workloads";
import { HistoryScreen } from "@/screens/History";
import { SettingsScreen } from "@/screens/Settings";
import { MigrationView } from "@/screens/MigrationView";
import { syncBackendConfig } from "@/lib/store";

const queryClient = new QueryClient({
  defaultOptions: {
    queries: {
      staleTime: 1_000,
      retry: false,
      refetchOnWindowFocus: false,
    },
  },
});

function App() {
  const [screen, setScreen] = useState<Screen>("dashboard");
  const [migrationID, setMigrationID] = useState<string | null>(null);
  const [returnScreen, setReturnScreen] = useState<Screen>("workloads");

  // The Rust side persists endpoints independently of localStorage; sync once
  // at startup so a fresh install picks up the saved control-plane URL.
  useEffect(() => {
    void syncBackendConfig();
  }, []);

  const openMigration = (id: string) => {
    setReturnScreen(screen);
    setMigrationID(id);
  };

  return (
    <Shell screen={screen} onNavigate={(next) => {
      setMigrationID(null);
      setScreen(next);
    }}>
      {migrationID ? (
        <MigrationView
          migrationID={migrationID}
          onClose={() => {
            setMigrationID(null);
            setScreen(returnScreen);
          }}
        />
      ) : screen === "dashboard" ? (
        <DashboardScreen />
      ) : screen === "machines" ? (
        <MachinesScreen />
      ) : screen === "workloads" ? (
        <WorkloadsScreen onMigrate={openMigration} />
      ) : screen === "history" ? (
        <HistoryScreen onOpenMigration={openMigration} />
      ) : (
        <SettingsScreen />
      )}
    </Shell>
  );
}

export default function Root() {
  return (
    <QueryClientProvider client={queryClient}>
      <App />
    </QueryClientProvider>
  );
}
