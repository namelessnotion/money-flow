# frozen_string_literal: true

# A market with the parts a Security needs: an Issuer, a Borrower, an Investor,
# a Security between them, and every Wallet each of those holds.
#
# Every securities spec needs the same cast, and naming them one `let` at a time
# spreads six or seven memoized helpers over each example group — which is both
# noisy to read and a real hazard, since a spec that forgets to touch the wallet
# helper gets a Security whose accounts were never opened. Building the whole
# thing in one object means a spec says which parts it cares about instead.
module SecuritiesWorld
  # The cast, plus wallet lookup by serialized account type.
  class World
    attr_reader :issuer, :borrower, :investor, :security

    def initialize(issuer:, borrower:, investor:, security:)
      @issuer = issuer
      @borrower = borrower
      @investor = investor
      @security = security
    end

    # Every account any party to this Security holds — what a shape is handed,
    # and what its scoped resolvers pick apart again.
    def accounts
      Models::Account.where(entity_id: [issuer.id, borrower.id, investor.id]).all
    end

    # A Security's own wallet, by type: 'security_supply' and friends.
    def security_wallet(type)
      account_for(nil, type) { |account| account.security_id == security.id }
    end

    # An entity's own wallet, by type.
    def wallet_of(entity, type)
      account_for(entity.id, type) { |account| account.security_id.nil? }
    end

    private

    def account_for(entity_id, type)
      match = accounts.find do |account|
        account.type == type && (entity_id.nil? || account.entity_id == entity_id) && yield(account)
      end
      raise "no #{type} account in this world" if match.nil?

      match.wallet_uuid
    end
  end

  # `security` takes any Models::Security attribute overrides — principal,
  # rate, a second Issuer — so a spec can shape the offering it needs.
  def securities_world(investor_role: Types::Enums::EntityRole::Investor, **security_attributes)
    issuer = create_provisioned_entity(role: Types::Enums::EntityRole::Issuer)
    borrower = create_provisioned_entity(role: Types::Enums::EntityRole::Borrower)
    investor = create_provisioned_entity(role: investor_role)
    security = create(:security, issuer: issuer, borrower: borrower, **security_attributes)
    open_security_wallets(issuer, security)

    World.new(issuer: issuer, borrower: borrower, investor: investor, security: security)
  end

  # The Wallets an offering opens, on its Issuer's Holder — the same list
  # Services::Securities::IssueOffering opens, so the two cannot drift.
  def open_security_wallets(issuer, security)
    Services::Securities::IssueOffering::WALLETS.each do |type|
      create(:account, entity: issuer, security: security, type: type.serialize)
    end
  end
end
