import { LoadingState } from "@/components/state";

export default function Loading() {
  return (
    <main className="flex min-h-[60vh] items-center justify-center px-6">
      <LoadingState label="LOADING" />
    </main>
  );
}
