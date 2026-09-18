# frozen_string_literal: true

require 'spec_helper'

RSpec.describe Services::TwirpCall do
  let(:failure) { Class.new(StandardError) }

  def ok
    Twirp::ClientResp.new(data: Holder::V1::ProvisionResponse.new)
  end

  def call_with(&)
    described_class.with_retries(failure: failure, &)
  end

  it 'returns the first successful response' do
    expect(call_with { ok }.data).to be_a(Holder::V1::ProvisionResponse)
  end

  it 'retries a retryable twirp code and returns the later success' do
    attempts = 0
    response = call_with do
      attempts += 1
      attempts == 1 ? Twirp::ClientResp.new(error: Twirp::Error.unavailable('later')) : ok
    end

    expect(attempts).to eq(2)
    expect(response.error).to be_nil
  end

  it 'raises the given failure at once for a non-retryable code' do
    attempts = 0
    expect do
      call_with do
        attempts += 1
        Twirp::ClientResp.new(error: Twirp::Error.invalid_argument('bad amount'))
      end
    end.to raise_error(failure, 'bad amount')
    expect(attempts).to eq(1)
  end

  it 'gives up after MAX_ATTEMPTS retryable errors' do
    attempts = 0
    expect do
      call_with do
        attempts += 1
        Twirp::ClientResp.new(error: Twirp::Error.unavailable('down'))
      end
    end.to raise_error(failure, 'down')
    expect(attempts).to eq(described_class::MAX_ATTEMPTS)
  end

  it 'retries transport failures and wraps the last one' do
    attempts = 0
    expect do
      call_with do
        attempts += 1
        raise Faraday::ConnectionFailed, 'connection refused'
      end
    end.to raise_error(failure, /connection refused/)
    expect(attempts).to eq(described_class::MAX_ATTEMPTS)
  end
end
