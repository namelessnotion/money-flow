# frozen_string_literal: true
# typed: strict

require 'faraday'
require 'twirp'

module Services
  # The one retry policy for calling the Go services over Twirp.
  #
  # Every Go RPC Ruby calls is idempotent per id, so any call that may not have
  # landed — a transport failure, or a twirp code saying the call may never
  # have reached the service or lost its reply — is safe to send again with the
  # same request. Anything else fails identically however many times it is
  # sent, so it is raised straight away.
  #
  # Domain rejections are not errors here: they arrive inside a successful
  # response, so unwrapping them stays with the caller that knows the oneof.
  module TwirpCall
    MAX_ATTEMPTS = 3

    RETRYABLE_CODES = T.let(%i[unavailable deadline_exceeded internal unknown].freeze, T::Array[Symbol])

    # Yields until the RPC returns a response without a twirp error, and
    # returns that response. Raises `failure` with the last error's message once
    # the error is not retryable or attempts run out.
    sig do
      params(failure: T.class_of(StandardError), blk: T.proc.returns(Twirp::ClientResp))
        .returns(Twirp::ClientResp)
    end
    def self.with_retries(failure:, &blk)
      attempt = 0
      loop do
        attempt += 1

        response = attempt(blk, failure, attempt)
        next if response.nil? # transport failure with attempts left

        error = response.error
        return response if error.nil?

        raise failure, error.msg unless attempt < MAX_ATTEMPTS && RETRYABLE_CODES.include?(error.code)
      end
    end

    # A transport failure never becomes a Twirp::Error — the client's Faraday
    # call raises straight through — but it is the same situation as a
    # retryable code: the request may never have arrived, or arrived and lost
    # its reply. Returns nil when it should be retried.
    sig do
      params(rpc: T.proc.returns(Twirp::ClientResp), failure: T.class_of(StandardError), attempt: Integer)
        .returns(T.nilable(Twirp::ClientResp))
    end
    def self.attempt(rpc, failure, attempt)
      rpc.call
    rescue Faraday::Error => e
      raise failure, e.message unless attempt < MAX_ATTEMPTS

      nil
    end
    private_class_method :attempt
  end
end
