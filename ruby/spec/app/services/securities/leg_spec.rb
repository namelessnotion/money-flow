# frozen_string_literal: true

require 'spec_helper'

RSpec.describe Services::Securities::Leg do
  describe '.cash_id_for' do
    let(:money_id) { '0199a1b2-0000-7000-8000-000000000001' }

    it "derives it from the money leg's id alone, so a re-sent Transaction converges in Go" do
      # The rows record only the money leg's id. Its cash leg is not a second
      # id to remember; it is a function of the first, the way Go's detid
      # would derive it.
      expect(described_class.cash_id_for(money_id)).to eq(Services::DetId.for("#{money_id}:cash"))
    end

    it 'is a different Transfer from the money leg it sits beside' do
      expect(described_class.cash_id_for(money_id)).not_to eq(money_id)
    end
  end

  describe '.money' do
    let(:from) { described_class::MoneyWallets.new(cleared: 'from-cleared', cash: 'from-cash') }
    let(:to) { described_class::MoneyWallets.new(cleared: 'to-cleared', cash: 'to-cash') }
    let(:legs) { described_class.money(id: 'money', amount_minor_units: 500, from: from, to: to) }

    it 'moves the amount on the cleared side under the given id' do
      leg = legs.fetch('money')

      expect([leg.from_wallet_id, leg.to_wallet_id]).to eq(%w[from-cleared to-cleared])
      expect(leg.amount.minor_units).to eq(500)
    end

    it 'moves the same amount between the same two parties on the cash side' do
      leg = legs.fetch(described_class.cash_id_for('money'))

      expect([leg.from_wallet_id, leg.to_wallet_id]).to eq(%w[from-cash to-cash])
      expect(leg.amount.minor_units).to eq(500)
    end

    it 'is exactly those two legs' do
      expect(legs.keys).to eq(['money', described_class.cash_id_for('money')])
    end
  end

  describe '.after' do
    it 'gates every child on the one parent' do
      dependency = described_class.after(%w[a b], 'root')

      expect(dependency.keys).to eq(%w[a b])
      expect(dependency.values.map(&:transfer_id)).to eq([['root'], ['root']])
    end
  end
end
