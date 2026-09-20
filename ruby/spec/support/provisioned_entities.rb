# frozen_string_literal: true

# An entity with the accounts onboarding would have opened for it — the same
# set Services::OnboardEntity plans from Types::Enums::AccountType.for_role.
#
# Specs used to build this inline by iterating every AccountType. That stopped
# being right twice over once Securities landed: a role no longer gets every
# type, and three of them belong to a Security rather than an entity, which the
# accounts_security_scoped_types CHECK refuses outright. Having one helper means
# the next change to what a role holds moves one line, not five specs.
module ProvisionedEntities
  # The entity, plus one account per type its role opens.
  def create_provisioned_entity(role: Types::Enums::EntityRole::Investor, **attributes)
    entity = create(:entity, role: role.serialize, **attributes)
    Types::Enums::AccountType.for_role(role).each do |type|
      create(:account, entity: entity, type: type.serialize)
    end
    entity
  end

  # Wallet uuid by serialized account type, for an entity built above.
  def wallet_uuids_of(entity)
    Models::Account.where(entity_id: entity.id).all.to_h { |account| [account.type, account.wallet_uuid] }
  end
end
