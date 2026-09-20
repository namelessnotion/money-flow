# frozen_string_literal: true
# typed: ignore

# What part an entity plays in this market. Exactly one per entity: the role
# decides which accounts onboarding opens for it, and an entity whose accounts
# nobody decided is not a thing this system can originate work for.
#
# Roles written out literally, not read from Types::Enums::EntityRole — a
# migration has to keep meaning what it meant when it ran.
#
# Backfilled to 'investor': every entity that exists predates this capability
# and was used for ACH only, which every role can do. The default is dropped
# straight after the backfill so a future insert has to say the role out loud.
#
# No index: three values over a small table, and every query that filters by
# role also filters by something far more selective.
ENTITY_ROLES = %w[investor borrower issuer].freeze

Sequel.migration do
  up do
    alter_table(:entities) do
      add_column :role, String, null: false, default: 'investor'
      set_column_default :role, nil
      add_constraint(:entities_role_check, role: ENTITY_ROLES)
    end
  end

  down do
    alter_table(:entities) do
      drop_constraint(:entities_role_check)
      drop_column :role
    end
  end
end
