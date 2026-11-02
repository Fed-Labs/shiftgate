import { LoadingState } from "@/components/state";

export default function Loading() {
  return (
    <div className="px-6 py-16">
      <LoadingState label="LOADING DASHBOARD" />
    </div>
  );
}
