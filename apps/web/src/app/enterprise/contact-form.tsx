import { Button } from "@/components/ui";
import { Mail } from "lucide-react";

export function EnterpriseContactForm() {
  return <div className="space-y-4"><p className="text-sm text-text-secondary">The website does not submit or store lead forms yet. Email the team to begin a private deployment or security review.</p><a href="mailto:hello@fed-labs.xyz?subject=SHIFT%20enterprise%20evaluation"><Button type="button" variant="primary"><Mail size={14} className="mr-2" /> Email enterprise</Button></a></div>;
}
