# frozen_string_literal: true

require 'spec_helper'

RSpec.describe Services::Securities::Wallets do
  let(:account_type) { Types::Enums::AccountType }
  let(:issuer) { create_provisioned_entity(role: Types::Enums::EntityRole::Issuer) }
  let(:investor) { create_provisioned_entity(role: Types::Enums::EntityRole::Investor) }
  let(:security) { create(:security, issuer: issuer) }

  # A Security's three wallets, opened on its Issuer's Holder.
  def open_security_wallets
    %w[security_supply security_escrow security_repayment].map do |type|
      create(:account, entity: issuer, security: security, type: type)
    end
  end

  describe '.of_entity' do
    it "answers with the wallet backing each of the entity's accounts" do
      accounts = Models::Account.where(entity_id: investor.id).all

      resolved = described_class.of_entity(investor.id, [account_type::ClearedCash, account_type::Investment], accounts)

      expect(resolved.keys).to contain_exactly(account_type::ClearedCash, account_type::Investment)
      expect(resolved.fetch(account_type::Investment))
        .to eq(accounts.find { |a| a.type == 'investment' }.wallet_uuid)
    end

    it "never answers with a Security's wallet" do
      # The whole point of the security_id column: an entity-scoped lookup must
      # not reach a wallet that belongs to an offering rather than the Issuer.
      open_security_wallets
      accounts = Models::Account.where(entity_id: issuer.id).all

      expect { described_class.of_entity(issuer.id, [account_type::SecuritySupply], accounts) }
        .to raise_error(Services::Securities::MissingAccount, /security_supply/)
    end

    it 'names every account it is missing, and whose it is' do
      accounts = Models::Account.where(entity_id: investor.id).all

      expect { described_class.of_entity(investor.id, [account_type::IssuerControl, account_type::Gain], accounts) }
        .to raise_error(Services::Securities::MissingAccount, /entity #{investor.id} has no issuer_control, gain/)
    end
  end

  describe '.of_security' do
    before { open_security_wallets }

    it "answers with the wallet backing each of the Security's accounts" do
      accounts = Models::Account.where(entity_id: issuer.id).all

      resolved = described_class.of_security(
        security.id, [account_type::SecuritySupply, account_type::SecurityEscrow], accounts
      )

      expect(resolved.keys).to contain_exactly(account_type::SecuritySupply, account_type::SecurityEscrow)
    end

    it "never answers with the issuer's own wallet, even though it is on the same holder" do
      accounts = Models::Account.where(entity_id: issuer.id).all

      expect { described_class.of_security(security.id, [account_type::IssuerControl], accounts) }
        .to raise_error(Services::Securities::MissingAccount, /issuer_control/)
    end

    it 'never answers with another security\'s wallet' do
      other = create(:security, issuer: issuer)
      accounts = Models::Account.where(entity_id: issuer.id).all

      expect { described_class.of_security(other.id, [account_type::SecuritySupply], accounts) }
        .to raise_error(Services::Securities::MissingAccount, /#{other.id}/)
    end

    it 'says which security it could not resolve for' do
      accounts = Models::Account.where(entity_id: issuer.id).all

      expect { described_class.of_security(security.id, [account_type::Cash], accounts) }
        .to raise_error(Services::Securities::MissingAccount, /security #{security.id}/)
    end
  end

  it 'raises rather than silently picking one when a scope holds two of a type' do
    # An entity may legitimately hold several banks, so a shape that resolves
    # by type would otherwise pick whichever the hash happened to keep last —
    # money moving through an arbitrary wallet, decided by iteration order.
    entity = create(:entity)
    2.times { create(:account, entity: entity, type: 'bank') }
    accounts = Models::Account.where(entity_id: entity.id).all

    expect { described_class.of_entity(entity.id, [account_type::Bank], accounts) }
      .to raise_error(Services::Securities::MissingAccount, /entity #{entity.id} has more than one bank/)
  end
end
