package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"text/tabwriter"
	"time"

	"shift.dev/shift/internal/billing"
	"shift.dev/shift/internal/controlclient"
	"shift.dev/shift/internal/oidc"
	"shift.dev/shift/internal/scheduler"
	"shift.dev/shift/internal/termio"
)

// control.go: the control-plane half of the CLI. Where the rest of the CLI
// talks to the local agent, these commands talk to the control plane — the
// fleet registry, entitlements, audit trail, API keys, and the compute
// marketplace — through a persisted session created by `shift login`.

// controlSession opens the session described by the global flags. It fails
// with a pointer to login rather than a bare "unauthorized" when no tokens
// are stored, because that is the actual next step for the operator.
func controlSession(settings options) (*controlclient.Session, error) {
	session, err := controlclient.NewSession(settings.controlURL, settings.tokenStorePath, settings.timeout)
	if err != nil {
		return nil, exitError{code: 2, err: err}
	}
	if !session.Authenticated() {
		return nil, exitError{code: 3, err: errors.New("not logged in; run shiftgate login first")}
	}
	return session, nil
}

// controlOperationError maps a control-plane failure onto the CLI's exit-code
// contract: 4 for a missing resource, 3 for every other operation failure.
func controlOperationError(err error) error {
	var api *controlclient.APIError
	if errors.As(err, &api) {
		if api.Status == 404 {
			return exitError{code: 4, err: err}
		}
		if api.Status == 401 {
			return exitError{code: 3, err: fmt.Errorf("%s (run shiftgate login)", api.Message)}
		}
	}
	return exitError{code: 3, err: err}
}

// resolveOrganization picks the organization fleet commands operate on: an
// explicit --org, the only one, or the first of several with a note that the
// flag exists.
func resolveOrganization(ctx context.Context, session *controlclient.Session, organizationID string) (string, error) {
	if organizationID != "" {
		return organizationID, nil
	}
	organizations, err := session.Organizations(ctx)
	if err != nil {
		return "", controlOperationError(err)
	}
	if len(organizations) == 0 {
		return "", exitError{code: 3, err: errors.New("this account belongs to no organization")}
	}
	return organizations[0].ID, nil
}

// runControlLogin exchanges credentials for a session and persists it. The
// password is never a flag — flags land in shell history and process
// listings — so it is read from the terminal (or a piped stdin) instead.
// --sso routes through the control plane's OIDC issuer instead: a loopback
// listener receives the authorization code, and no password is ever read.
func runControlLogin(ctx context.Context, settings options, arguments []string) error {
	flags := flag.NewFlagSet("login", flag.ContinueOnError)
	flags.SetOutput(settings.stderr)
	email := flags.String("email", "", "account email (prompted when omitted)")
	sso := flags.Bool("sso", false, "log in through the control plane's single sign-on issuer")
	if err := flags.Parse(arguments); err != nil {
		return usageError(err.Error())
	}
	// No control-plane check here: an unset --control-url means the platform's
	// control plane, which is the normal case for a hosted deployment.
	reader := bufio.NewReader(os.Stdin)
	if *email == "" {
		_, _ = fmt.Fprint(settings.stdout, "Email: ")
		value, err := reader.ReadString('\n')
		if err != nil {
			return exitError{code: 3, err: fmt.Errorf("read email: %w", err)}
		}
		*email = strings.TrimSpace(value)
	}
	if *email == "" {
		return usageError("an email is required")
	}
	session, err := controlclient.NewSession(settings.controlURL, settings.tokenStorePath, settings.timeout)
	if err != nil {
		return exitError{code: 2, err: err}
	}
	if *sso {
		return runControlLoginSSO(ctx, settings, session, *email)
	}
	_, _ = fmt.Fprint(settings.stdout, "Password: ")
	password, err := termio.ReadPassword(os.Stdin)
	if err != nil {
		return exitError{code: 3, err: fmt.Errorf("read password: %w", err)}
	}
	if password == "" {
		return usageError("a password is required")
	}
	user, err := session.Login(ctx, *email, password)
	if err != nil {
		return controlOperationError(err)
	}
	if settings.jsonOutput {
		return writeJSON(settings.stdout, map[string]any{"email": user.Email, "control_plane": session.Client().URL()})
	}
	_, _ = fmt.Fprintf(settings.stdout, "Logged in as %s (%s). Session stored in %s.\n", user.DisplayName, user.Email, session.TokenStorePath())
	return nil
}

// runControlLoginSSO performs the browser-based single sign-on flow: start a
// loopback listener, ask the control plane for the issuer's authorization URL,
// open the browser, and redeem the code the listener receives. The verifier
// never leaves this process, and the code travels only over the loopback
// interface.
func runControlLoginSSO(ctx context.Context, settings options, session *controlclient.Session, email string) error {
	verifier, challenge, err := oidc.Verifier()
	if err != nil {
		return exitError{code: 3, err: fmt.Errorf("generate PKCE verifier: %w", err)}
	}
	listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		return exitError{code: 3, err: fmt.Errorf("start loopback listener: %w", err)}
	}
	defer func() { _ = listener.Close() }()
	redirectURI := "http://" + listener.Addr().String() + "/callback"
	authorization, err := session.Client().BeginSSO(ctx, email, redirectURI, challenge)
	if err != nil {
		return controlOperationError(err)
	}
	code, failure := listenForSSOCode(listener, authorization.State)
	_, _ = fmt.Fprintf(settings.stdout, "Continue in your browser:\n\n  %s\n\n", authorization.AuthorizationURL)
	if launcher, lookupErr := exec.LookPath("xdg-open"); lookupErr == nil {
		_ = exec.CommandContext(context.Background(), launcher, authorization.AuthorizationURL).Start()
	}
	select {
	case received := <-code:
		user, err := session.LoginSSO(ctx, received, authorization.State, verifier, redirectURI)
		if err != nil {
			return controlOperationError(err)
		}
		if settings.jsonOutput {
			return writeJSON(settings.stdout, map[string]any{"email": user.Email, "control_plane": session.Client().URL()})
		}
		_, _ = fmt.Fprintf(settings.stdout, "Logged in as %s (%s). Session stored in %s.\n", user.DisplayName, user.Email, session.TokenStorePath())
		return nil
	case message := <-failure:
		return exitError{code: 3, err: errors.New(message)}
	case <-time.After(5 * time.Minute):
		return exitError{code: 3, err: errors.New("single sign-on timed out after five minutes")}
	case <-ctx.Done():
		return exitError{code: 3, err: ctx.Err()}
	}
}

// listenForSSOCode serves the loopback redirect: the issuer sends the browser
// here with the authorization code and the state that must match the one the
// flow started with. The browser gets a plain page to close; the channels
// carry the outcome back to the waiting login.
func listenForSSOCode(listener net.Listener, state string) (code, failure chan string) {
	code = make(chan string, 1)
	failure = make(chan string, 1)
	server := &http.Server{Handler: http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/callback" {
			http.NotFound(writer, request)
			return
		}
		query := request.URL.Query()
		switch {
		case query.Get("error") != "":
			failure <- "the issuer reported " + query.Get("error")
			writer.WriteHeader(http.StatusBadRequest)
			_, _ = fmt.Fprint(writer, "Login failed. You can close this tab.")
		case query.Get("state") != state:
			failure <- "the issuer returned an unexpected state; start the login again"
			writer.WriteHeader(http.StatusBadRequest)
			_, _ = fmt.Fprint(writer, "Login failed. You can close this tab.")
		case query.Get("code") != "":
			code <- query.Get("code")
			_, _ = fmt.Fprint(writer, "Login complete. You can close this tab and return to the terminal.")
		default:
			failure <- "the issuer returned no authorization code"
			writer.WriteHeader(http.StatusBadRequest)
			_, _ = fmt.Fprint(writer, "Login failed. You can close this tab.")
		}
	})}
	go func() { _ = server.Serve(listener) }()
	go func() {
		<-time.After(10 * time.Minute)
		_ = server.Close()
	}()
	return code, failure
}

// runControl routes the control-plane commands.
func runControl(ctx context.Context, command string, arguments []string, settings options) error {
	switch command {
	case "login":
		return runControlLogin(ctx, settings, arguments)
	case "logout":
		return runControlLogout(ctx, settings)
	case "whoami":
		return runControlWhoami(ctx, settings)
	case "plans":
		return runControlPlans(ctx, settings)
	case "storage":
		return runControlStorage(ctx, settings, arguments)
	case "fleet":
		return runFleet(ctx, settings, arguments)
	case "marketplace", "market":
		return runMarketplace(ctx, settings, arguments)
	default:
		return usageError("unknown command " + command)
	}
}

// runControlLogout revokes the session server-side and removes the token file.
func runControlLogout(ctx context.Context, settings options) error {
	session, err := controlSession(settings)
	if err != nil {
		return err
	}
	if err := session.Logout(ctx); err != nil {
		return controlOperationError(err)
	}
	if settings.jsonOutput {
		return writeJSON(settings.stdout, map[string]string{"status": "logged out"})
	}
	_, _ = fmt.Fprintln(settings.stdout, "Logged out.")
	return nil
}

// runControlWhoami shows the identity and organizations the session carries.
func runControlWhoami(ctx context.Context, settings options) error {
	session, err := controlSession(settings)
	if err != nil {
		return err
	}
	organizations, err := session.Organizations(ctx)
	if err != nil {
		return controlOperationError(err)
	}
	if settings.jsonOutput {
		return writeJSON(settings.stdout, map[string]any{"email": session.UserEmail(), "control_plane": session.Client().URL(), "organizations": organizations})
	}
	_, _ = fmt.Fprintf(settings.stdout, "%s on %s\n", session.UserEmail(), session.Client().URL())
	for _, organization := range organizations {
		_, _ = fmt.Fprintf(settings.stdout, "  %s  %s (%s)\n", organization.ID, organization.Name, organization.Role)
	}
	return nil
}

// runControlPlans prints the public plan catalog. Pricing and limits are
// public by design — the catalog is what the entitlement checks enforce.
func runControlPlans(ctx context.Context, settings options) error {
	client, err := controlclient.New(settings.controlURL, settings.timeout)
	if err != nil {
		return exitError{code: 2, err: err}
	}
	var plans []billing.Plan
	if err := client.Plans(ctx, &plans); err != nil {
		return controlOperationError(err)
	}
	if settings.jsonOutput {
		return writeJSON(settings.stdout, plans)
	}
	writer := tabwriter.NewWriter(settings.stdout, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(writer, "KEY\tNAME\tPRICE\tMACHINES\tSTORAGE\tFEATURES")
	for _, plan := range plans {
		price := "free"
		if plan.PriceCents > 0 {
			price = fmt.Sprintf("$%d/mo", plan.PriceCents/100)
		}
		_, _ = fmt.Fprintf(writer, "%s\t%s\t%s\t%d\t%s\t%s\n", plan.Key, plan.Name, price, plan.MaxMachines, humanBytes(plan.MaxStorageBytes), strings.Join(plan.Features, ", "))
	}
	return writer.Flush()
}

// runControlStorage prints the organization's hosted storage status plus the
// exact agent configuration lines that enable hosted mirroring — so an
// operator can copy them straight into an agent's environment.
func runControlStorage(ctx context.Context, settings options, arguments []string) error {
	flags := flag.NewFlagSet("storage", flag.ContinueOnError)
	flags.SetOutput(settings.stderr)
	organizationID := flags.String("org", "", "organization id (defaults to the session's only organization)")
	if err := flags.Parse(arguments); err != nil {
		return usageError(err.Error())
	}
	if flags.NArg() > 0 {
		return usageError("storage takes no positional arguments")
	}
	session, err := controlSession(settings)
	if err != nil {
		return err
	}
	resolved, err := resolveOrganization(ctx, session, *organizationID)
	if err != nil {
		return err
	}
	status, err := session.Storage(ctx, resolved)
	if err != nil {
		return controlOperationError(err)
	}
	if settings.jsonOutput {
		return writeJSON(settings.stdout, status)
	}
	_, _ = fmt.Fprintf(settings.stdout, "Organization %s\n", resolved)
	if !status.Enabled {
		_, _ = fmt.Fprintln(settings.stdout, "Hosted storage: disabled — this control plane does not host checkpoint storage.")
		_, _ = fmt.Fprintln(settings.stdout, "Agents keep checkpoints locally; configure an S3 object store on the agent for a cloud copy.")
		return nil
	}
	overQuota := ""
	if status.OverQuota {
		overQuota = "  (OVER QUOTA — credential issuance suspended)"
	}
	_, _ = fmt.Fprintf(settings.stdout, "Hosted storage: enabled%s\n", overQuota)
	_, _ = fmt.Fprintf(settings.stdout, "  Location:     %s / %s (prefix %s)\n", status.Endpoint, status.Bucket, status.Prefix)
	_, _ = fmt.Fprintf(settings.stdout, "  Usage:        %s / %s\n", humanBytes(status.UsedStorageBytes), humanBytes(status.MaxStorageBytes))
	if status.LastReconciledAt != nil {
		_, _ = fmt.Fprintf(settings.stdout, "  Reconciled:   %s\n", status.LastReconciledAt.Local().Format(time.DateTime))
	}
	_, _ = fmt.Fprintln(settings.stdout)
	_, _ = fmt.Fprintln(settings.stdout, "Agent setup — add to the agent's environment for hosted mirroring:")
	_, _ = fmt.Fprintln(settings.stdout, "  SHIFT_OBJECTSTORE_ENABLED=true")
	_, _ = fmt.Fprintln(settings.stdout, "  SHIFT_OBJECTSTORE_BACKEND=control-plane")
	_, _ = fmt.Fprintf(settings.stdout, "  SHIFT_CONTROL_ORGANIZATION_ID=%s\n", resolved)
	_, _ = fmt.Fprintln(settings.stdout, "  SHIFT_CONTROL_MACHINE_ID=<this machine's id>")
	_, _ = fmt.Fprintln(settings.stdout, "  SHIFT_CONTROL_API_KEY=<machines-scope api key>")
	_, _ = fmt.Fprintln(settings.stdout, "Agents fetch short-lived scoped credentials from the control plane; no storage secrets are configured on the agent.")
	_, _ = fmt.Fprintln(settings.stdout, "(No SHIFT_CONTROL_URL needed — unset, agents talk to the platform endpoint.)")
	return nil
}

// runFleet routes the fleet subcommands: the registry the control plane keeps
// about machines, workloads, migrations, checkpoints, audit, keys, and usage.
// The --org flag comes before the subcommand (Go's flag package stops at the
// first non-flag word, and each subcommand owns its own flags after it).
func runFleet(ctx context.Context, settings options, arguments []string) error {
	flags := flag.NewFlagSet("fleet", flag.ContinueOnError)
	flags.SetOutput(settings.stderr)
	organization := flags.String("org", "", "organization id (defaults to your first organization)")
	if err := flags.Parse(arguments); err != nil {
		return usageError(err.Error())
	}
	remaining := flags.Args()
	if len(remaining) == 0 {
		return usageError("usage: shiftgate fleet [--org ID] machines|workloads|migrations|checkpoints|audit|retention|sso|api-keys|entitlement|usage [args]")
	}
	subcommand, args := remaining[0], remaining[1:]
	session, err := controlSession(settings)
	if err != nil {
		return err
	}
	organizationID, err := resolveOrganization(ctx, session, *organization)
	if err != nil {
		return err
	}
	switch subcommand {
	case "machines":
		return runFleetMachine(ctx, session, settings, organizationID, args)
	case "workloads":
		return runFleetWorkload(ctx, session, settings, organizationID, args)
	case "migrations":
		return runFleetMigration(ctx, session, settings, organizationID, args)
	case "checkpoints":
		return runFleetCheckpoint(ctx, session, settings, organizationID, args)
	case "audit":
		events, err := session.AuditEvents(ctx, organizationID, 0)
		if err != nil {
			return controlOperationError(err)
		}
		return printAuditEvents(settings, events)
	case "api-keys":
		return runFleetAPIKeys(ctx, session, settings, organizationID, args)
	case "retention":
		return runFleetRetention(ctx, session, settings, organizationID, args)
	case "sso":
		return runFleetSSO(ctx, session, settings, organizationID, args)
	case "entitlement":
		entitlement, err := session.Entitlement(ctx, organizationID)
		if err != nil {
			return controlOperationError(err)
		}
		return printEntitlement(settings, entitlement)
	case "usage":
		return runFleetUsage(ctx, session, settings, organizationID, args)
	default:
		return usageError("unknown fleet subcommand " + subcommand)
	}
}

// runFleetMachines backs the top-level `shift machines` in fleet mode.
func runFleetMachines(ctx context.Context, settings options, arguments []string) error {
	session, err := controlSession(settings)
	if err != nil {
		return err
	}
	flags := flag.NewFlagSet("machines", flag.ContinueOnError)
	flags.SetOutput(settings.stderr)
	organization := flags.String("org", "", "organization id (defaults to your first organization)")
	if err := flags.Parse(arguments); err != nil {
		return usageError(err.Error())
	}
	organizationID, err := resolveOrganization(ctx, session, *organization)
	if err != nil {
		return err
	}
	return runFleetMachine(ctx, session, settings, organizationID, flags.Args())
}

// runFleetMachine lists (by default), registers, and queries machines. The
// capability subcommand is the fleet query: which machines report a given
// capability, through the projection the control plane maintains.
func runFleetMachine(ctx context.Context, session *controlclient.Session, settings options, organizationID string, arguments []string) error {
	subcommand := "list"
	if len(arguments) > 0 {
		subcommand = arguments[0]
	}
	if subcommand != "list" && subcommand != "register" && subcommand != "capability" {
		return usageError("unknown machines subcommand " + subcommand)
	}
	args := []string{}
	if len(arguments) > 1 {
		args = arguments[1:]
	}
	switch subcommand {
	case "list":
		machines, err := session.Machines(ctx, organizationID)
		if err != nil {
			return controlOperationError(err)
		}
		return printFleetMachines(settings, machines)
	case "capability":
		return runFleetMachineCapability(ctx, session, settings, organizationID, args)
	case "register":
		flags := flag.NewFlagSet("machines register", flag.ContinueOnError)
		flags.SetOutput(settings.stderr)
		machineID := flags.String("machine-id", "", "stable machine id")
		name := flags.String("name", "", "display name")
		agentURL := flags.String("agent-url", "", "https agent URL peers and the dispatcher dial")
		if err := flags.Parse(args); err != nil {
			return usageError(err.Error())
		}
		if *machineID == "" || *name == "" || *agentURL == "" {
			return usageError("machine-id, name, and agent-url are required")
		}
		machine, err := session.RegisterMachine(ctx, organizationID, controlclient.CreateMachineInput{MachineID: *machineID, Name: *name, AgentURL: *agentURL})
		if err != nil {
			return controlOperationError(err)
		}
		if settings.jsonOutput {
			return writeJSON(settings.stdout, machine)
		}
		_, _ = fmt.Fprintf(settings.stdout, "Registered %s (%s).\n", machine.MachineID, machine.ID)
		return nil
	}
	return nil
}

// runFleetMachineCapability asks which machines report a capability. The kind
// defaults to text; --minimum applies to numbers and filters the floor, so
// `shift fleet machines capability cuda_version --kind number --minimum 9`
// answers "which machines can run a CUDA 12 build".
func runFleetMachineCapability(ctx context.Context, session *controlclient.Session, settings options, organizationID string, arguments []string) error {
	flags := flag.NewFlagSet("machines capability", flag.ContinueOnError)
	flags.SetOutput(settings.stderr)
	kind := flags.String("kind", "text", "capability kind: number, text, or boolean")
	minimum := flags.Float64("minimum", 0, "minimum value, numbers only")
	if err := flags.Parse(arguments); err != nil {
		return usageError(err.Error())
	}
	rest := flags.Args()
	if len(rest) != 1 {
		return usageError("usage: shiftgate fleet machines capability NAME [--kind number] [--minimum 9.0]")
	}
	if *kind != "number" && *kind != "text" && *kind != "boolean" {
		return usageError("kind must be number, text, or boolean")
	}
	if *minimum != 0 && *kind != "number" {
		return usageError("--minimum applies to numbers only")
	}
	machines, err := session.MachinesWithCapability(ctx, organizationID, rest[0], *kind, *minimum)
	if err != nil {
		return controlOperationError(err)
	}
	if len(machines) == 0 {
		_, _ = fmt.Fprintf(settings.stderr, "no machines report capability %q\n", rest[0])
		return nil
	}
	return printFleetMachines(settings, machines)
}

// runFleetRetention shows the organization's retention windows and, when any
// --*-days flag is set, updates them first. Unset flags leave their window
// unchanged; zero means keep forever and must be spelled explicitly.
func runFleetRetention(ctx context.Context, session *controlclient.Session, settings options, organizationID string, arguments []string) error {
	flags := flag.NewFlagSet("retention", flag.ContinueOnError)
	flags.SetOutput(settings.stderr)
	auditDays := flags.Int("audit-days", -1, "audit log retention in days (0 keeps forever)")
	checkpointDays := flags.Int("checkpoint-days", -1, "checkpoint retention in days (0 keeps forever)")
	storageDays := flags.Int("deleted-storage-days", -1, "grace period for deleted storage objects in days (0 keeps forever)")
	if err := flags.Parse(arguments); err != nil {
		return usageError(err.Error())
	}
	if flags.NArg() > 0 {
		return usageError("usage: shiftgate fleet retention [--audit-days N] [--checkpoint-days N] [--deleted-storage-days N]")
	}
	// -1 is "flag not set"; 0 is the real keep-forever policy and passes through.
	if *auditDays >= 0 || *checkpointDays >= 0 || *storageDays >= 0 {
		update := controlclient.RetentionPolicyUpdate{}
		if *auditDays >= 0 {
			update.AuditRetentionDays = auditDays
		}
		if *checkpointDays >= 0 {
			update.CheckpointRetentionDays = checkpointDays
		}
		if *storageDays >= 0 {
			update.DeletedStorageRetentionDays = storageDays
		}
		if _, err := session.SetRetentionPolicy(ctx, organizationID, update); err != nil {
			return controlOperationError(err)
		}
	}
	policy, err := session.RetentionPolicy(ctx, organizationID)
	if err != nil {
		return controlOperationError(err)
	}
	if settings.jsonOutput {
		return writeJSON(settings.stdout, policy)
	}
	writer := tabwriter.NewWriter(settings.stdout, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(writer, "ORGANIZATION\tAUDIT\tCHECKPOINTS\tDELETED STORAGE\tUPDATED")
	_, _ = fmt.Fprintf(writer, "%s\t%s\t%s\t%s\t%s\n", shortID(policy.OrganizationID), windowOrForever(policy.AuditRetentionDays), windowOrForever(policy.CheckpointRetentionDays), windowOrForever(policy.DeletedStorageRetentionDays), policy.UpdatedAt.Local().Format(time.DateTime))
	return writer.Flush()
}

// windowOrForever renders a retention window; zero is the deliberate
// keep-forever policy and says so rather than printing a bare 0.
func windowOrForever(days int) string {
	if days == 0 {
		return "forever"
	}
	return fmt.Sprintf("%dd", days)
}

// runFleetSSO shows the organization's SSO enforcement state and, with
// --enforce DOMAIN or --disable, changes it. Enforcing claims an email domain:
// from that moment the domain's users log in through the control plane's OIDC
// issuer and passwords stop working for them.
func runFleetSSO(ctx context.Context, session *controlclient.Session, settings options, organizationID string, arguments []string) error {
	flags := flag.NewFlagSet("sso", flag.ContinueOnError)
	flags.SetOutput(settings.stderr)
	enforce := flags.String("enforce", "", "enforce single sign-on for an email domain (e.g. example.com)")
	disable := flags.Bool("disable", false, "disable enforcement and release the claimed domain")
	if err := flags.Parse(arguments); err != nil {
		return usageError(err.Error())
	}
	if flags.NArg() > 0 {
		return usageError("usage: shiftgate fleet sso [--enforce DOMAIN | --disable]")
	}
	if *enforce != "" && *disable {
		return usageError("pick one: --enforce DOMAIN or --disable")
	}
	if *enforce != "" {
		if _, err := session.SetOrganizationSSO(ctx, organizationID, true, *enforce); err != nil {
			return controlOperationError(err)
		}
	} else if *disable {
		if _, err := session.SetOrganizationSSO(ctx, organizationID, false, ""); err != nil {
			return controlOperationError(err)
		}
	}
	state, err := session.OrganizationSSO(ctx, organizationID)
	if err != nil {
		return controlOperationError(err)
	}
	if settings.jsonOutput {
		return writeJSON(settings.stdout, state)
	}
	status := "off"
	if state.Enforced {
		status = "enforced for " + state.EmailDomain
	}
	_, _ = fmt.Fprintf(settings.stdout, "single sign-on: %s\n", status)
	if state.Enforced {
		_, _ = fmt.Fprintln(settings.stdout, "the domain's accounts authenticate with `shiftgate login --sso`; passwords no longer work for them")
	}
	return nil
}

// runFleetWorkloads backs the top-level `shift workloads` in fleet mode.
func runFleetWorkloads(ctx context.Context, settings options, arguments []string) error {
	session, err := controlSession(settings)
	if err != nil {
		return err
	}
	flags := flag.NewFlagSet("workloads", flag.ContinueOnError)
	flags.SetOutput(settings.stderr)
	organization := flags.String("org", "", "organization id (defaults to your first organization)")
	if err := flags.Parse(arguments); err != nil {
		return usageError(err.Error())
	}
	organizationID, err := resolveOrganization(ctx, session, *organization)
	if err != nil {
		return err
	}
	return runFleetWorkload(ctx, session, settings, organizationID, flags.Args())
}

// runFleetWorkload lists workloads.
func runFleetWorkload(ctx context.Context, session *controlclient.Session, settings options, organizationID string, arguments []string) error {
	if len(arguments) > 0 {
		return usageError("usage: shiftgate fleet workloads (listing only; create workloads on the machine)")
	}
	workloads, err := session.Workloads(ctx, organizationID)
	if err != nil {
		return controlOperationError(err)
	}
	if settings.jsonOutput {
		return writeJSON(settings.stdout, workloads)
	}
	writer := tabwriter.NewWriter(settings.stdout, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(writer, "ID\tNAME\tMACHINE\tUPDATED")
	for _, workload := range workloads {
		_, _ = fmt.Fprintf(writer, "%s\t%s\t%s\t%s\n", shortID(workload.ID), workload.Name, workload.MachineID, workload.UpdatedAt.Local().Format(time.DateTime))
	}
	return writer.Flush()
}

// runFleetMigration lists migrations and inspects one job with its events.
func runFleetMigration(ctx context.Context, session *controlclient.Session, settings options, organizationID string, arguments []string) error {
	if len(arguments) == 0 {
		jobs, err := session.Migrations(ctx, organizationID)
		if err != nil {
			return controlOperationError(err)
		}
		if settings.jsonOutput {
			return writeJSON(settings.stdout, jobs)
		}
		writer := tabwriter.NewWriter(settings.stdout, 0, 4, 2, ' ', 0)
		_, _ = fmt.Fprintln(writer, "ID\tWORKLOAD\tMODE\tSTATUS\tSOURCE\tDESTINATION\tUPDATED")
		for _, job := range jobs {
			_, _ = fmt.Fprintf(writer, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", shortID(job.ID), job.WorkloadID, job.Mode, job.Status, shortID(job.SourceMachineID), shortID(job.DestinationMachineID), job.UpdatedAt.Local().Format(time.DateTime))
		}
		return writer.Flush()
	}
	if len(arguments) != 1 {
		return usageError("usage: shiftgate fleet migrations [ID]")
	}
	migrationID := arguments[0]
	jobs, err := session.Migrations(ctx, organizationID)
	if err != nil {
		return controlOperationError(err)
	}
	var found *controlclient.MigrationJob
	for index := range jobs {
		if jobs[index].ID == migrationID {
			found = &jobs[index]
			break
		}
	}
	if found == nil {
		return exitError{code: 4, err: fmt.Errorf("migration %s not found", migrationID)}
	}
	events, err := session.MigrationEvents(ctx, organizationID, migrationID)
	if err != nil {
		return controlOperationError(err)
	}
	if settings.jsonOutput {
		return writeJSON(settings.stdout, map[string]any{"migration": found, "events": events})
	}
	printControlMigration(settings, *found)
	for _, event := range events {
		_, _ = fmt.Fprintf(settings.stdout, "  %s  %3.0f%%  %s\n", event.CreatedAt.Local().Format(time.DateTime), event.Progress*100, event.Message)
	}
	return nil
}

// runFleetCheckpoint lists checkpoints, optionally for one workload.
func runFleetCheckpoint(ctx context.Context, session *controlclient.Session, settings options, organizationID string, arguments []string) error {
	if len(arguments) > 1 {
		return usageError("usage: shiftgate fleet checkpoints [WORKLOAD]")
	}
	workloadID := ""
	if len(arguments) == 1 {
		workloadID = arguments[0]
	}
	checkpoints, err := session.Checkpoints(ctx, organizationID, workloadID)
	if err != nil {
		return controlOperationError(err)
	}
	if settings.jsonOutput {
		return writeJSON(settings.stdout, checkpoints)
	}
	writer := tabwriter.NewWriter(settings.stdout, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(writer, "ID\tWORKLOAD\tMACHINE\tKIND\tPLAIN\tSTORED\tCREATED")
	for _, checkpoint := range checkpoints {
		_, _ = fmt.Fprintf(writer, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", shortID(checkpoint.ID), shortID(checkpoint.WorkloadID), shortID(checkpoint.MachineID), checkpoint.Kind, humanBytes(checkpoint.PlainBytes), humanBytes(checkpoint.StoredBytes), checkpoint.CreatedAt.Local().Format(time.DateTime))
	}
	return writer.Flush()
}

// runFleetUsage summarizes metered usage; from and to are inclusive ISO dates.
func runFleetUsage(ctx context.Context, session *controlclient.Session, settings options, organizationID string, arguments []string) error {
	flags := flag.NewFlagSet("fleet usage", flag.ContinueOnError)
	flags.SetOutput(settings.stderr)
	from := flags.String("from", "", "range start, 2006-01-02 (defaults to a month ago)")
	to := flags.String("to", "", "range end, 2006-01-02 (defaults to tomorrow)")
	if err := flags.Parse(arguments); err != nil {
		return usageError(err.Error())
	}
	var fromTime, toTime time.Time
	if *from != "" {
		parsed, err := time.Parse("2006-01-02", *from)
		if err != nil {
			return usageError("--from must be an ISO date, 2006-01-02")
		}
		fromTime = parsed
	}
	if *to != "" {
		parsed, err := time.Parse("2006-01-02", *to)
		if err != nil {
			return usageError("--to must be an ISO date, 2006-01-02")
		}
		toTime = parsed
	}
	summaries, err := session.Usage(ctx, organizationID, fromTime, toTime)
	if err != nil {
		return controlOperationError(err)
	}
	if settings.jsonOutput {
		return writeJSON(settings.stdout, summaries)
	}
	if len(summaries) == 0 {
		_, _ = fmt.Fprintln(settings.stdout, "No metered usage in this period.")
		return nil
	}
	writer := tabwriter.NewWriter(settings.stdout, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(writer, "KIND\tQUANTITY\tPERIOD")
	for _, summary := range summaries {
		_, _ = fmt.Fprintf(writer, "%s\t%d\t%s to %s\n", summary.Kind, summary.Quantity, summary.PeriodStart, summary.PeriodEnd)
	}
	return writer.Flush()
}

// runFleetAPIKeys lists, creates, and revokes organization API keys.
func runFleetAPIKeys(ctx context.Context, session *controlclient.Session, settings options, organizationID string, arguments []string) error {
	if len(arguments) == 0 {
		keys, err := session.APIKeys(ctx, organizationID)
		if err != nil {
			return controlOperationError(err)
		}
		if settings.jsonOutput {
			return writeJSON(settings.stdout, keys)
		}
		writer := tabwriter.NewWriter(settings.stdout, 0, 4, 2, ' ', 0)
		_, _ = fmt.Fprintln(writer, "ID\tNAME\tPREFIX\tSCOPES\tSTATUS")
		for _, key := range keys {
			status := "active"
			if key.RevokedAt != nil {
				status = "revoked"
			}
			_, _ = fmt.Fprintf(writer, "%s\t%s\t%s\t%s\t%s\n", shortID(key.ID), key.Name, key.Prefix, strings.Join(key.Scopes, ","), status)
		}
		return writer.Flush()
	}
	subcommand, args := arguments[0], arguments[1:]
	switch subcommand {
	case "create":
		flags := flag.NewFlagSet("fleet api-keys create", flag.ContinueOnError)
		flags.SetOutput(settings.stderr)
		name := flags.String("name", "", "key name")
		scopes := flags.String("scopes", "machines,workloads,checkpoints,migrations", "comma-separated scopes")
		if err := flags.Parse(args); err != nil {
			return usageError(err.Error())
		}
		if *name == "" {
			return usageError("--name is required")
		}
		created, err := session.CreateAPIKey(ctx, organizationID, controlclient.CreateAPIKeyInput{Name: *name, Scopes: strings.Split(*scopes, ",")})
		if err != nil {
			return controlOperationError(err)
		}
		if settings.jsonOutput {
			return writeJSON(settings.stdout, created)
		}
		_, _ = fmt.Fprintf(settings.stdout, "API key %s created. Secret (shown once, store it now):\n  %s\n", created.Name, created.Secret)
		return nil
	case "revoke":
		if len(args) != 1 {
			return usageError("usage: shiftgate fleet api-keys revoke KEY")
		}
		if err := session.RevokeAPIKey(ctx, organizationID, args[0]); err != nil {
			return controlOperationError(err)
		}
		_, _ = fmt.Fprintln(settings.stdout, "Revoked.")
		return nil
	default:
		return usageError("unknown api-keys subcommand " + subcommand)
	}
}

// runMarketplace routes the compute marketplace: publishing and withdrawing
// offers, inspecting inventory, placements, and reservations. As with fleet,
// --org comes before the subcommand.
func runMarketplace(ctx context.Context, settings options, arguments []string) error {
	flags := flag.NewFlagSet("marketplace", flag.ContinueOnError)
	flags.SetOutput(settings.stderr)
	organization := flags.String("org", "", "organization id (defaults to your first organization)")
	if err := flags.Parse(arguments); err != nil {
		return usageError(err.Error())
	}
	remaining := flags.Args()
	if len(remaining) == 0 {
		return usageError("usage: shiftgate marketplace [--org ID] offers|inventory|place|publish|withdraw|reservations|reserve|commit|release|fail [args]")
	}
	subcommand, args := remaining[0], remaining[1:]
	session, err := controlSession(settings)
	if err != nil {
		return err
	}
	organizationID, err := resolveOrganization(ctx, session, *organization)
	if err != nil {
		return err
	}
	switch subcommand {
	case "offers":
		offers, err := session.ComputeOffers(ctx, organizationID)
		if err != nil {
			return controlOperationError(err)
		}
		return printComputeOffers(settings, offers)
	case "inventory":
		inventory, err := session.ComputeInventory(ctx, organizationID)
		if err != nil {
			return controlOperationError(err)
		}
		return printComputeInventory(settings, inventory)
	case "place":
		return runMarketplacePlace(ctx, session, settings, organizationID, args)
	case "publish":
		return runMarketplacePublish(ctx, session, settings, organizationID, args)
	case "withdraw":
		return runMarketplaceWithdraw(ctx, session, settings, organizationID, args)
	case "reservations":
		return runMarketplaceReservations(ctx, session, settings, organizationID, args)
	case "reserve":
		return runMarketplaceReserve(ctx, session, settings, organizationID, args)
	case "commit", "release", "fail":
		return runMarketplaceTransition(ctx, session, settings, organizationID, subcommand, args)
	default:
		return usageError("unknown marketplace subcommand " + subcommand)
	}
}

// resourceFlags registers the requirement flags common to placements and
// reservations — cpu (cores), memory and storage (bytes or sizes like 8GiB) —
// and returns a function turning the parsed values into scheduler Resources,
// with state-bytes carried separately for policy checks.
func resourceFlags(flags *flag.FlagSet) func() (scheduler.Resources, uint64, error) {
	cpu := flags.Float64("cpu", 0, "CPU cores required")
	memory := flags.String("memory", "", "memory required (bytes or 8GiB)")
	storage := flags.String("storage", "", "storage required (bytes or 100GiB)")
	stateBytes := flags.String("state-bytes", "", "checkpoint size (bytes or 4GiB)")
	return func() (scheduler.Resources, uint64, error) {
		resources := scheduler.Resources{CPUCount: *cpu}
		if *memory != "" {
			value, err := parseBytes(*memory)
			if err != nil {
				return scheduler.Resources{}, 0, fmt.Errorf("--memory: %w", err)
			}
			resources.MemoryBytes = uint64(value)
		}
		if *storage != "" {
			value, err := parseBytes(*storage)
			if err != nil {
				return scheduler.Resources{}, 0, fmt.Errorf("--storage: %w", err)
			}
			resources.StorageBytes = uint64(value)
		}
		var state uint64
		if *stateBytes != "" {
			value, err := parseBytes(*stateBytes)
			if err != nil {
				return scheduler.Resources{}, 0, fmt.Errorf("--state-bytes: %w", err)
			}
			state = uint64(value)
		}
		return resources, state, nil
	}
}

// runMarketplacePlace ranks destinations for a workload. It commits nothing —
// the point is to see what the scheduler would choose and why it rejected the
// rest.
func runMarketplacePlace(ctx context.Context, session *controlclient.Session, settings options, organizationID string, arguments []string) error {
	flags := flag.NewFlagSet("marketplace place", flag.ContinueOnError)
	flags.SetOutput(settings.stderr)
	workload := flags.String("workload", "", "workload being placed")
	checkpoint := flags.String("checkpoint", "", "id of the checkpoint whose manifest gates restore compatibility")
	source := flags.String("source", "", "source machine id (excluded from candidates, used for locality)")
	duration := flags.Int64("duration", 0, "planned duration in seconds")
	limit := flags.Int("limit", 5, "maximum candidates to return")
	regions := flags.String("regions", "", "comma-separated allowed regions")
	countries := flags.String("countries", "", "comma-separated allowed ISO country codes")
	trust := flags.String("min-trust", "", "minimum trust tier (unverified, verified, audited)")
	maxPrice := flags.Int64("max-price", 0, "maximum price in micros per hour (1000000 = one unit)")
	requirements := resourceFlags(flags)
	if err := flags.Parse(arguments); err != nil {
		return usageError(err.Error())
	}
	resources, stateBytes, err := requirements()
	if err != nil {
		return usageError(err.Error())
	}
	input := controlclient.PlacementInput{
		WorkloadID:      *workload,
		CheckpointID:    *checkpoint,
		SourceMachineID: *source,
		Requirements:    resources,
		StateBytes:      stateBytes,
		DurationSeconds: *duration,
		Limit:           *limit,
	}
	if *regions != "" {
		input.Constraints.AllowedRegions = strings.Split(*regions, ",")
	}
	if *countries != "" {
		input.Constraints.AllowedCountries = strings.Split(*countries, ",")
	}
	if *trust != "" {
		input.Constraints.MinTrust = scheduler.TrustTier(*trust)
	}
	if *maxPrice > 0 {
		input.Constraints.MaxPriceMicrosPerHour = *maxPrice
	}
	placement, err := session.PlaceCompute(ctx, organizationID, input)
	if err != nil {
		return controlOperationError(err)
	}
	if settings.jsonOutput {
		return writeJSON(settings.stdout, placement)
	}
	_, _ = fmt.Fprintf(settings.stdout, "Evaluated %d offers (trading %s).\n", placement.Evaluated, enabledDisabled(placement.TradingEnabled))
	if len(placement.Candidates) == 0 {
		_, _ = fmt.Fprintln(settings.stdout, "No destination can take this workload.")
	} else {
		_, _ = fmt.Fprintln(settings.stdout, "Candidates:")
		for index, candidate := range placement.Candidates {
			_, _ = fmt.Fprintf(settings.stdout, "  %d. %s  %s  %s\n", index+1, candidate.MachineID, candidate.MachineName, candidateCurrency(candidate.HourlyMicros, candidate.Currency))
		}
	}
	if len(placement.Rejected) > 0 {
		_, _ = fmt.Fprintln(settings.stdout, "Rejected:")
		for _, rejection := range placement.Rejected {
			_, _ = fmt.Fprintf(settings.stdout, "  %s  %s: %s\n", rejection.MachineID, rejection.Code, rejection.Reason)
		}
	}
	return nil
}

// runMarketplacePublish exposes a registered machine's resources. Machine
// identity, agent URL, and hardware come from the control plane's registry,
// so this only names the machine and the terms.
func runMarketplacePublish(ctx context.Context, session *controlclient.Session, settings options, organizationID string, arguments []string) error {
	flags := flag.NewFlagSet("marketplace publish", flag.ContinueOnError)
	flags.SetOutput(settings.stderr)
	machine := flags.String("machine", "", "machine id to expose")
	region := flags.String("region", "", "region the machine runs in")
	country := flags.String("country", "", "ISO 3166-1 alpha-2 country code")
	zone := flags.String("zone", "", "zone inside the region")
	visibility := flags.String("visibility", "organization", "who may schedule: organization, partners, or public (public requires trading)")
	currency := flags.String("currency", "", "ISO 4217 currency for the prices (empty means free)")
	cpuHour := flags.Int64("cpu-hour-micros", 0, "price per CPU hour, micros (1000000 = one unit)")
	memoryGiBHour := flags.Int64("memory-gib-hour-micros", 0, "price per GiB-hour of memory, micros")
	storageGiBHour := flags.Int64("storage-gib-hour-micros", 0, "price per GiB-hour of storage, micros")
	gpuHour := flags.Int64("gpu-hour-micros", 0, "price per GPU hour, micros")
	maxState := flags.String("max-state-bytes", "", "largest checkpoint the offer accepts (bytes or 4GiB)")
	maxDuration := flags.Int64("max-duration-hours", 0, "longest migration the offer accepts, hours")
	requireEncrypted := flags.Bool("require-encrypted-state", false, "refuse unencrypted state")
	exposed := resourceFlags(flags)
	if err := flags.Parse(arguments); err != nil {
		return usageError(err.Error())
	}
	if *machine == "" {
		return usageError("--machine is required")
	}
	resources, _, err := exposed()
	if err != nil {
		return usageError(err.Error())
	}
	input := controlclient.PublishOfferInput{
		MachineID: *machine,
		Exposed:   resources,
		Pricing: scheduler.Pricing{
			Currency:             *currency,
			CPUHourMicros:        *cpuHour,
			MemoryGiBHourMicros:  *memoryGiBHour,
			StorageGiBHourMicros: *storageGiBHour,
			GPUHourMicros:        *gpuHour,
		},
		Geography: scheduler.Geography{Region: *region, Country: *country, Zone: *zone},
		Policy: scheduler.Policy{
			Visibility:            scheduler.Visibility(*visibility),
			MaxDurationHours:      *maxDuration,
			RequireEncryptedState: *requireEncrypted,
		},
	}
	if *maxState != "" {
		value, err := parseBytes(*maxState)
		if err != nil {
			return usageError("--max-state-bytes: " + err.Error())
		}
		input.Policy.MaxStateBytes = uint64(value)
	}
	offer, err := session.PublishComputeOffer(ctx, organizationID, input)
	if err != nil {
		return controlOperationError(err)
	}
	if settings.jsonOutput {
		return writeJSON(settings.stdout, offer)
	}
	_, _ = fmt.Fprintf(settings.stdout, "Published offer %s for machine %s.\n", offer.ID, offer.MachineID)
	return nil
}

// runMarketplaceWithdraw takes an offer off the market.
func runMarketplaceWithdraw(ctx context.Context, session *controlclient.Session, settings options, organizationID string, arguments []string) error {
	flags := flag.NewFlagSet("marketplace withdraw", flag.ContinueOnError)
	flags.SetOutput(settings.stderr)
	offer := flags.String("offer", "", "offer id")
	if err := flags.Parse(arguments); err != nil {
		return usageError(err.Error())
	}
	if *offer == "" {
		return usageError("--offer is required")
	}
	withdrawn, err := session.WithdrawComputeOffer(ctx, organizationID, *offer)
	if err != nil {
		return controlOperationError(err)
	}
	if settings.jsonOutput {
		return writeJSON(settings.stdout, withdrawn)
	}
	_, _ = fmt.Fprintf(settings.stdout, "Withdrew offer %s.\n", withdrawn.ID)
	return nil
}

// runMarketplaceReservations lists live holds, optionally for one offer.
func runMarketplaceReservations(ctx context.Context, session *controlclient.Session, settings options, organizationID string, arguments []string) error {
	flags := flag.NewFlagSet("marketplace reservations", flag.ContinueOnError)
	flags.SetOutput(settings.stderr)
	offer := flags.String("offer", "", "filter to this offer id")
	if err := flags.Parse(arguments); err != nil {
		return usageError(err.Error())
	}
	reservations, err := session.ComputeReservations(ctx, organizationID, *offer)
	if err != nil {
		return controlOperationError(err)
	}
	if settings.jsonOutput {
		return writeJSON(settings.stdout, reservations)
	}
	if len(reservations) == 0 {
		_, _ = fmt.Fprintln(settings.stdout, "No reservations.")
		return nil
	}
	writer := tabwriter.NewWriter(settings.stdout, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(writer, "ID\tOFFER\tMACHINE\tWORKLOAD\tSTATE\tEXPIRES")
	for _, reservation := range reservations {
		_, _ = fmt.Fprintf(writer, "%s\t%s\t%s\t%s\t%s\t%s\n", shortID(reservation.ID), shortID(reservation.OfferID), reservation.MachineID, reservation.WorkloadID, string(reservation.State), reservation.ExpiresAt.Local().Format(time.DateTime))
	}
	return writer.Flush()
}

// runMarketplaceReserve holds capacity on an offer. The control plane re-runs
// the scheduler server-side, so a hold can never be granted on terms a
// placement would refuse.
func runMarketplaceReserve(ctx context.Context, session *controlclient.Session, settings options, organizationID string, arguments []string) error {
	flags := flag.NewFlagSet("marketplace reserve", flag.ContinueOnError)
	flags.SetOutput(settings.stderr)
	offer := flags.String("offer", "", "offer id to hold capacity on")
	workload := flags.String("workload", "", "workload the hold is for")
	checkpoint := flags.String("checkpoint", "", "id of the checkpoint whose manifest gates restore compatibility")
	source := flags.String("source", "", "source machine id")
	duration := flags.Int64("duration", 0, "planned duration in seconds")
	ttl := flags.Int64("ttl", 0, "how long the hold lasts, seconds (server default when 0)")
	requirements := resourceFlags(flags)
	if err := flags.Parse(arguments); err != nil {
		return usageError(err.Error())
	}
	if *offer == "" {
		return usageError("--offer is required")
	}
	resources, stateBytes, err := requirements()
	if err != nil {
		return usageError(err.Error())
	}
	reservation, err := session.CreateComputeReservation(ctx, organizationID, controlclient.CreateReservationInput{
		OfferID:         *offer,
		WorkloadID:      *workload,
		CheckpointID:    *checkpoint,
		SourceMachineID: *source,
		Requested:       resources,
		StateBytes:      stateBytes,
		DurationSeconds: *duration,
		TTLSeconds:      *ttl,
	})
	if err != nil {
		return controlOperationError(err)
	}
	if settings.jsonOutput {
		return writeJSON(settings.stdout, reservation)
	}
	_, _ = fmt.Fprintf(settings.stdout, "Reserved %s on offer %s (machine %s) until %s.\n", reservation.ID, reservation.OfferID, reservation.MachineID, reservation.ExpiresAt.Local().Format(time.DateTime))
	_, _ = fmt.Fprintln(settings.stdout, "Commit it when the migration lands; release it otherwise.")
	return nil
}

// runMarketplaceTransition moves a reservation through its state machine.
func runMarketplaceTransition(ctx context.Context, session *controlclient.Session, settings options, organizationID, action string, arguments []string) error {
	flags := flag.NewFlagSet("marketplace "+action, flag.ContinueOnError)
	flags.SetOutput(settings.stderr)
	reservationID := flags.String("reservation", "", "reservation id")
	reason := flags.String("reason", "", "why the reservation failed (fail only)")
	if err := flags.Parse(arguments); err != nil {
		return usageError(err.Error())
	}
	if *reservationID == "" {
		return usageError("--reservation is required")
	}
	var state scheduler.ReservationState
	switch action {
	case "commit":
		state = scheduler.ReservationActive
	case "release":
		state = scheduler.ReservationReleased
	case "fail":
		state = scheduler.ReservationFailed
	}
	reservation, err := session.SetComputeReservationState(ctx, organizationID, *reservationID, state, *reason)
	if err != nil {
		return controlOperationError(err)
	}
	if settings.jsonOutput {
		return writeJSON(settings.stdout, reservation)
	}
	_, _ = fmt.Fprintf(settings.stdout, "Reservation %s is %s.\n", reservation.ID, string(reservation.State))
	return nil
}

// printFleetMachines renders the machine registry.
func printFleetMachines(settings options, machines []controlclient.Machine) error {
	if settings.jsonOutput {
		return writeJSON(settings.stdout, machines)
	}
	writer := tabwriter.NewWriter(settings.stdout, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(writer, "MACHINE\tNAME\tSTATUS\tLAST SEEN\tAGENT")
	for _, machine := range machines {
		lastSeen := "never"
		if machine.LastSeenAt != nil {
			lastSeen = machine.LastSeenAt.Local().Format(time.DateTime)
		}
		_, _ = fmt.Fprintf(writer, "%s\t%s\t%s\t%s\t%s\n", machine.MachineID, machine.Name, machine.Status, lastSeen, machine.AgentURL)
	}
	return writer.Flush()
}

// printAuditEvents renders the audit trail, newest first.
func printAuditEvents(settings options, events []controlclient.AuditEvent) error {
	if settings.jsonOutput {
		return writeJSON(settings.stdout, events)
	}
	writer := tabwriter.NewWriter(settings.stdout, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(writer, "AT\tACTION\tRESOURCE\tACTOR")
	for _, event := range events {
		_, _ = fmt.Fprintf(writer, "%s\t%s\t%s/%s\t%s\n", event.CreatedAt.Local().Format(time.DateTime), event.Action, event.ResourceType, shortID(event.ResourceID), shortID(event.ActorUserID))
	}
	return writer.Flush()
}

// printEntitlement renders the plan and its remaining allowance.
func printEntitlement(settings options, entitlement controlclient.Entitlement) error {
	if settings.jsonOutput {
		return writeJSON(settings.stdout, entitlement)
	}
	_, _ = fmt.Fprintf(settings.stdout, "Plan:      %s (%s)\n", entitlement.Plan, entitlement.Status)
	_, _ = fmt.Fprintf(settings.stdout, "Machines:  %d\n", entitlement.MaxMachines)
	_, _ = fmt.Fprintf(settings.stdout, "Storage:   %s of %s used\n", humanBytes(entitlement.UsedStorageBytes), humanBytes(entitlement.MaxStorageBytes))
	return nil
}

// printComputeOffers renders the organization's own offers, withdrawn included.
func printComputeOffers(settings options, offers []controlclient.ComputeOffer) error {
	if settings.jsonOutput {
		return writeJSON(settings.stdout, offers)
	}
	writer := tabwriter.NewWriter(settings.stdout, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(writer, "OFFER\tMACHINE\tCPU\tMEMORY\tSTATUS")
	for _, offer := range offers {
		status := string(offer.Availability.Status)
		if offer.WithdrawnAt != nil {
			status = "withdrawn"
		}
		_, _ = fmt.Fprintf(writer, "%s\t%s\t%.1f\t%s\t%s\n", shortID(offer.ID), offer.MachineID, offer.Exposed.CPUCount, humanBytes(int64(offer.Exposed.MemoryBytes)), status)
	}
	return writer.Flush()
}

// printComputeInventory renders everything schedulable right now, including
// the trading gate that explains what is missing.
func printComputeInventory(settings options, inventory controlclient.ComputeInventory) error {
	if settings.jsonOutput {
		return writeJSON(settings.stdout, inventory)
	}
	_, _ = fmt.Fprintf(settings.stdout, "Public trading: %s. %d offer(s) schedulable.\n", enabledDisabled(inventory.TradingEnabled), len(inventory.Offers))
	writer := tabwriter.NewWriter(settings.stdout, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(writer, "OFFER\tMACHINE\tTRUST\tCPU\tMEMORY\tPRICE")
	for _, offer := range inventory.Offers {
		_, _ = fmt.Fprintf(writer, "%s\t%s\t%s\t%.1f\t%s\t%s\n", shortID(offer.ID), offer.MachineName, string(offer.Trust), offer.Available.CPUCount, humanBytes(int64(offer.Available.MemoryBytes)), candidateCurrency(offer.Pricing.CPUHourMicros, offer.Pricing.Currency))
	}
	return writer.Flush()
}

// printControlMigration renders one control-plane migration job. settings is
// the output plumbing the caller already holds.
func printControlMigration(settings options, job controlclient.MigrationJob) {
	_, _ = fmt.Fprintf(settings.stdout, "Migration %s: %s %s -> %s (%s mode)\n", job.ID, job.Status, job.SourceMachineID, job.DestinationMachineID, job.Mode)
	if job.ErrorMessage != "" {
		_, _ = fmt.Fprintf(settings.stdout, "  error: %s\n", job.ErrorMessage)
	}
}

// enabledDisabled spells a boolean out for human output.
func enabledDisabled(value bool) string {
	if value {
		return "enabled"
	}
	return "disabled"
}

// candidateCurrency formats a per-hour micros price; a free offer says so
// rather than showing a meaningless "$0.000000".
func candidateCurrency(micros int64, currency string) string {
	if micros <= 0 {
		return "free"
	}
	if currency == "" {
		currency = "USD"
	}
	return fmt.Sprintf("%s %d.%06d/h", currency, micros/1_000_000, micros%1_000_000)
}
