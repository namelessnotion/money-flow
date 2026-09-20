# frozen_string_literal: true

require 'spec_helper'

RSpec.describe Services::OnboardEntity do
  # A real client, never actually connected to over the network in these
  # specs — just a concrete instance of the sig-required type so we can
  # stub #provision on it (an instance_double won't satisfy the sig, since
  # sorbet-runtime checks the argument via is_a?).
  subject(:service) { described_class.new(holder_client: holder_client) }

  let(:holder_client) { Holder::V1::HolderServiceClient.new('http://holder.internal.test') }
  let(:role) { Types::Enums::EntityRole::Investor }
  let(:request) { OnboardEntityRequest.new(name: 'Test Entity', role: role) }
  let(:account_types) { Types::Enums::AccountType.for_role(role) }

  # The dev database is shared with the end-to-end smoke run, so absolute counts
  # are meaningless here — only what this example changed matters.
  def local_rows
    { entities: Models::Entity.count, accounts: Models::Account.count }
  end

  def provisioned_response
    Twirp::ClientResp.new(data: Holder::V1::ProvisionResponse.new)
  end

  # The account types onboarding actually opened for an entity of this role.
  def account_types_onboarded(role)
    response = service.call(request: OnboardEntityRequest.new(name: 'Test Entity', role: role))
    Models::Account.where(entity_id: response.entity_id).all.map(&:type)
  end

  describe '#call' do
    context 'when provisioning succeeds' do
      before do
        allow(holder_client).to receive(:provision).and_return(provisioned_response)
      end

      it 'creates the entity with the holder uuid it provisioned' do
        response = service.call(request: request)

        expect(response).to be_a(OnboardEntityResponse)
        expect(response.success).to be true

        entity = Models::Entity[response.entity_id]
        expect(entity.name).to eq('Test Entity')
        expect(entity.holder_uuid).to eq(response.holder_uuid)
      end

      it 'creates one account per account type the role opens' do
        response = service.call(request: request)

        accounts = Models::Account.where(entity_id: response.entity_id).all
        expect(accounts.size).to eq(account_types.size)
        expect(accounts.map(&:type)).to match_array(account_types.map(&:serialize))
        expect(accounts.map(&:wallet_uuid).uniq.size).to eq(account_types.size)
      end

      it 'records the role it opened those accounts for' do
        response = service.call(request: request)

        expect(Models::Entity[response.entity_id].role).to eq('investor')
      end

      it 'opens an issuer somewhere to mint claim supply from, and nowhere to hold claims' do
        types = account_types_onboarded(Types::Enums::EntityRole::Issuer)

        expect(types).to include('issuer_control')
        expect(types).not_to include('investment')
      end

      it "never opens a Security's own wallets, which belong to an offering rather than an entity" do
        Types::Enums::EntityRole.each_value do |role|
          expect(account_types_onboarded(role))
            .not_to include('security_supply', 'security_escrow', 'security_repayment')
        end
      end

      it 'backs every account with the wallet uuid it sent for provisioning' do
        sent = nil
        allow(holder_client).to receive(:provision) do |req|
          sent = req
          provisioned_response
        end

        response = service.call(request: request)

        expect(sent.id).to eq(response.holder_uuid)
        expect(sent.wallets.size).to eq(account_types.size)

        # Each account's wallet_uuid must be one the service actually asked the
        # holder to open, or the account references a wallet that never existed.
        sent_by_name = sent.wallets.to_h { |spec| [spec.name, spec.wallet_id] }
        Models::Account.where(entity_id: response.entity_id).each do |account|
          expect(account.wallet_uuid).to eq(sent_by_name[account.type])
        end
      end

      it 'sends each account type its own allows policy' do
        sent = nil
        allow(holder_client).to receive(:provision) do |req|
          sent = req
          provisioned_response
        end

        service.call(request: request)

        allows_by_name = sent.wallets.to_h { |spec| [spec.name, spec.allows] }
        expect(allows_by_name['bank']).to eq(:ALLOWS_ONRAMP_AND_OFFRAMP)
        expect(allows_by_name['bank_control']).to eq(:ALLOWS_ONRAMP_AND_OFFRAMP)
        expect(allows_by_name['debit_card']).to eq(:ALLOWS_ONRAMP)
        expect(allows_by_name['cash']).to eq(:ALLOWS_NONE)
        # Never unspecified — the wallet service rejects an unset policy.
        expect(allows_by_name.values).not_to include(:ALLOWS_UNSPECIFIED)
      end

      it 'provisions before writing anything locally' do
        before = local_rows
        rows_at_provision_time = nil
        allow(holder_client).to receive(:provision) do
          rows_at_provision_time = local_rows
          provisioned_response
        end

        service.call(request: request)

        # Nothing local exists until the wallets do, so a local row can never
        # reference a wallet that was never opened.
        expect(rows_at_provision_time).to eq(before)
      end
    end

    context 'when provisioning fails' do
      before do
        allow(holder_client).to receive(:provision).and_return(
          Twirp::ClientResp.new(error: Twirp::Error.permission_denied('nope'))
        )
      end

      it 'raises without creating the entity or any accounts' do
        before = local_rows

        expect { service.call(request: request) }
          .to raise_error(described_class::ProvisioningFailed, 'nope')

        expect(Models::Entity.where(name: 'Test Entity')).to be_empty
        expect(local_rows).to eq(before)
      end
    end

    context 'when the holder refuses the request' do
      before do
        allow(holder_client).to receive(:provision).and_return(
          Twirp::ClientResp.new(
            data: Holder::V1::ProvisionResponse.new(
              holder_provision_rejected: Holder::V1::HolderProvisionRejected.new(reason: 'wallet taken')
            )
          )
        )
      end

      it 'raises rather than treating a domain rejection as success' do
        expect { service.call(request: request) }
          .to raise_error(described_class::ProvisioningFailed, 'wallet taken')

        expect(Models::Entity.where(name: 'Test Entity')).to be_empty
      end
    end

    context 'when provisioning is transiently unavailable' do
      it 'retries with the same uuids so the holder converges instead of orphaning' do
        ids = []
        call = 0
        allow(holder_client).to receive(:provision) do |req|
          call += 1
          ids << req.id
          call == 1 ? Twirp::ClientResp.new(error: Twirp::Error.unavailable('later')) : provisioned_response
        end

        response = service.call(request: request)

        expect(call).to eq(2)
        # Regenerating uuids on retry is what would leave a second, permanently
        # orphaned holder in the event log.
        expect(ids.uniq.size).to eq(1)
        expect(response.holder_uuid).to eq(ids.first)
      end

      # A connection failure never becomes a Twirp::Error — Faraday raises
      # straight out of the client — so it would otherwise escape both the retry
      # and the ProvisioningFailed wrapping entirely.
      it 'retries a transport failure and wraps it once attempts run out' do
        before = local_rows
        allow(holder_client).to receive(:provision).and_raise(
          Faraday::ConnectionFailed.new('connection refused')
        )

        expect { service.call(request: request) }
          .to raise_error(described_class::ProvisioningFailed, /connection refused/)

        expect(holder_client).to have_received(:provision).exactly(described_class::MAX_ATTEMPTS).times
        expect(Models::Entity.where(name: 'Test Entity')).to be_empty
        expect(local_rows).to eq(before)
      end

      it 'recovers when a transport failure clears on a later attempt' do
        call = 0
        allow(holder_client).to receive(:provision) do
          call += 1
          raise Faraday::ConnectionFailed, 'connection refused' if call == 1

          provisioned_response
        end

        expect { service.call(request: request) }.not_to raise_error
        expect(call).to eq(2)
      end

      it 'gives up after the attempt limit' do
        allow(holder_client).to receive(:provision).and_return(
          Twirp::ClientResp.new(error: Twirp::Error.unavailable('down'))
        )

        expect { service.call(request: request) }
          .to raise_error(described_class::ProvisioningFailed, 'down')

        expect(holder_client).to have_received(:provision).exactly(described_class::MAX_ATTEMPTS).times
      end
    end

    context 'when an account insert fails partway through' do
      before do
        allow(holder_client).to receive(:provision).and_return(provisioned_response)
      end

      it 'rolls back the entity and every account already inserted' do
        before = local_rows
        calls = 0
        allow(Models::Account).to receive(:create).and_wrap_original do |original, *args, **kwargs|
          calls += 1
          raise Sequel::DatabaseError, 'insert exploded' if calls == 4

          original.call(*args, **kwargs)
        end

        expect { service.call(request: request) }.to raise_error(Sequel::DatabaseError)

        # A half-provisioned entity is exactly what must not survive.
        expect(Models::Entity.where(name: 'Test Entity')).to be_empty
        expect(local_rows).to eq(before)
      end
    end
  end
end
