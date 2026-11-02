import { Button } from "@/components/ui";
import { Mail } from "lucide-react";

export function ComputeNotifyForm() {
  return <div className="space-y-3"><p className="text-sm text-text-secondary">The compute marketplace is experimental and not accepting sign-ups from this site.</p><a href="mailto:hello@fed-labs.xyz?subject=SHIFT%20compute%20marketplace"><Button type="button" variant="primary" size="sm"><Mail size={14} className="mr-2" /> Ask about the roadmap</Button></a></div>;
}
