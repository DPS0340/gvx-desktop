package ext

import (
	"fmt"
	"slices"
)

// Explicit extension permissions declared in extension.toml.
const (
	PermStorage      = "storage"
	PermSecrets      = "secrets"
	PermDocker       = "docker"
	PermSandboxes    = "sandboxes"
	PermSnapshots    = "snapshots"
	PermUIPickFolder = "ui:pick-folder"
	PermUITerminal   = "ui:terminal"
	PermUIOpenURL    = "ui:open-url"
	PermLocalProxy   = "localproxy"
	PermNetwork      = "network"
)

// AllPermissions lists all valid explicit permissions in canonical order.
var AllPermissions = []string{
	PermStorage,
	PermSecrets,
	PermDocker,
	PermSandboxes,
	PermSnapshots,
	PermUIPickFolder,
	PermUITerminal,
	PermUIOpenURL,
	PermLocalProxy,
	PermNetwork,
}

// ImplicitPermissions are available to every extension without declaration
// and are rejected if listed in extension.toml.
var ImplicitPermissions = []string{
	"host",
	"host:info",
	"views",
	"settings",
	"logs",
	"ui:toast",
	"none",
}

// CheckPermissions validates the permission list from extension.toml:
// at most MaxPermissions entries, no implicit permissions, no unknown names,
// and no duplicates.
func CheckPermissions(perms []string) ([]string, error) {
	if len(perms) > MaxPermissions {
		return nil, fmt.Errorf("more than %d permissions", MaxPermissions)
	}
	out := make([]string, 0, len(perms))
	for _, p := range perms {
		if slices.Contains(ImplicitPermissions, p) {
			return nil, fmt.Errorf("permission %q is implicit; leave it out", p)
		}
		if !slices.Contains(AllPermissions, p) {
			return nil, fmt.Errorf("unknown permission %q", p)
		}
		if slices.Contains(out, p) {
			return nil, fmt.Errorf("permission %q declared twice", p)
		}
		out = append(out, p)
	}
	return out, nil
}

// PermissionRisk returns "high", "medium", or "low" for UI risk badges.
func PermissionRisk(perm string) string {
	switch perm {
	case PermDocker, PermSandboxes:
		return "high"
	case PermSecrets, PermSnapshots, PermUITerminal, PermLocalProxy, PermNetwork:
		return "medium"
	case PermStorage, PermUIPickFolder, PermUIOpenURL:
		return "low"
	default:
		return "unknown"
	}
}

// PermissionDescription returns a short human-readable summary for a permission.
func PermissionDescription(perm string) string {
	switch perm {
	case PermStorage:
		return "Read and write per-extension key-value storage"
	case PermSecrets:
		return "Store and read encrypted secrets in the OS keychain vault"
	case PermDocker:
		return "Access the scoped Docker Engine API for extension containers"
	case PermSandboxes:
		return "Create, control, and execute commands in extension-owned sandboxes"
	case PermSnapshots:
		return "Create and restore checkpoint snapshots of extension-owned sandboxes"
	case PermUIPickFolder:
		return "Prompt you to choose a host folder on user interaction"
	case PermUITerminal:
		return "Open an interactive terminal to an extension container or sandbox"
	case PermUIOpenURL:
		return "Open external http(s) URLs in your default browser"
	case PermLocalProxy:
		return "Expose a web UI at https://<ext-id>.localhost via the local proxy"
	case PermNetwork:
		return "Access the external network from the extension runtime"
	default:
		return ""
	}
}
