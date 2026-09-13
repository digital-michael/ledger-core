package ledgercore

import "context"

// One place where a write can be refused.
//
// Some operations are ordinary work: retitling a ticket, moving it along,
// commenting on it. Others carry more weight -- changing what a ticket *is*
// after others have filed against it, deleting a project, changing many
// tickets at once, or rewriting words someone else wrote. With a single
// operator the distinction is academic; with several people and several
// agents it is the whole ballgame.
//
// Phase 2 (2026-09-12) builds the seam, not the policy: every privileged
// operation passes through Permit, and the default permits everything, which
// is exactly today's behaviour. When roles or per-project access arrive, they
// attach here rather than being retrofitted across every write path.
type Operation string

const (
	OpChangeItemType   Operation = "change_item_type"    // what a ticket is, after the fact
	OpReparentItem     Operation = "reparent_item"       // restructures the hierarchy
	OpDeleteProject    Operation = "delete_project"      // affects every ticket in it
	OpRestoreProject   Operation = "restore_project"     //
	OpBulkUpdateStatus Operation = "bulk_update_status"  // one action, many tickets
	OpBulkRelate       Operation = "bulk_relate"         //
	OpEditOthersNote   Operation = "edit_others_note"    // rewriting another actor's words
	OpDeleteOthersNote Operation = "delete_others_note"  //
)

// Target is what the operation acts on. ProjectID is here from the start
// because per-project access ("who may write to stock-manager") cannot be
// expressed without it, and adding it later would mean changing every call
// site. It is empty when the operation isn't scoped to one project, or when
// establishing it would cost a query the check doesn't otherwise need.
type Target struct {
	ProjectID  string
	EntityType string
	EntityID   string
}

// permit asks the configured policy. No policy means allow, which keeps a
// single-operator ledger working exactly as it does today.
func (db *DB) permit(ctx context.Context, op Operation, target Target) error {
	if db.policy == nil {
		return nil
	}
	return db.policy(ctx, db.currentActor(ctx), op, target)
}
