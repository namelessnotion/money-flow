# frozen_string_literal: true

# Real Twirp clients for the Go Transaction and Transfer services, never
# connected over the network — sorbet-runtime checks sig'd arguments with
# is_a?, so an instance_double won't do — plus canned responses for stubbing
# their RPCs.
module GoStubs
  def transaction_client
    @transaction_client ||= Transaction::V1::TransactionServiceClient.new('http://go.internal.test/twirp')
  end

  def transfer_client
    @transfer_client ||= Transfer::V1::TransferServiceClient.new('http://go.internal.test/twirp')
  end

  def holder_client
    @holder_client ||= Holder::V1::HolderServiceClient.new('http://go.internal.test/twirp')
  end

  def go_gateway
    Services::Ach::GoGateway.new(transaction_client: transaction_client, transfer_client: transfer_client)
  end

  def securities_gateway
    Services::Securities::GoGateway.new(transaction_client: transaction_client, holder_client: holder_client)
  end

  def twirp_ok(data) = Twirp::ClientResp.new(data: data)

  def transaction_initialized(id)
    twirp_ok(Transaction::V1::StartInitializingTransactionResponse.new(
               id: id, transaction_initialized: Transaction::V1::TransactionInitialized.new(id: id)
             ))
  end

  def transfer_pending(id)
    twirp_ok(Transfer::V1::ConfirmStagedTransferResponse.new(
               id: id, transfer_pending: Transfer::V1::TransferPending.new(id: id)
             ))
  end

  def transfer_committed(id)
    twirp_ok(Transfer::V1::PostPendingTransferResponse.new(
               id: id, transfer_committed: Transfer::V1::TransferCommitted.new(id: id)
             ))
  end

  def transfer_cancelled(id)
    twirp_ok(Transfer::V1::CancelStagedTransferResponse.new(
               id: id, transfer_cancelled: Transfer::V1::TransferCancelled.new(id: id)
             ))
  end

  def transaction_state(id, state)
    twirp_ok(Transaction::V1::GetTransactionStateResponse.new(id: id, state: state))
  end

  def transaction_rolled_back(id, reason)
    twirp_ok(Transaction::V1::GetTransactionStateResponse.new(
               id: id, state: :TRANSACTION_STATE_ROLLED_BACK, reason: reason
             ))
  end

  # Go's accept-time refusal: a well-formed Transaction it declined, which
  # arrives inside a successful response rather than as an error.
  def transaction_rejected(id, reason)
    twirp_ok(Transaction::V1::StartInitializingTransactionResponse.new(
               id: id,
               transaction_rejected: Transaction::V1::TransactionRejected.new(id: id, reason: reason)
             ))
  end

  def holder_provisioned = twirp_ok(Holder::V1::ProvisionResponse.new)

  # Stubs every Go RPC an ACH Transaction's life touches with its happy-path
  # answer, echoing the id each request was sent with.
  def stub_go_happy_path
    stub_transaction_service_happy_path
    stub_transfer_service_happy_path
  end

  # STARTED is what Submit requires before an entry may reach the provider —
  # the one place anything still asks Go directly. Nothing else does: since the
  # async cutover every other service learns an outcome from the projection.
  def stub_transaction_service_happy_path
    allow(transaction_client).to receive(:start_initializing_transaction) { |req| transaction_initialized(req.id) }
    allow(transaction_client).to receive(:get_transaction_state) do |req|
      transaction_state(req.id, :TRANSACTION_STATE_STARTED)
    end
  end

  # The same, for a Security's Transactions.
  def stub_go_securities_happy_path
    stub_go_happy_path
    allow(holder_client).to receive(:provision) { holder_provisioned }
  end

  def stub_transfer_service_happy_path
    { confirm_staged_transfer: :transfer_pending,
      post_pending_transfer: :transfer_committed,
      cancel_staged_transfer: :transfer_cancelled }.each do |rpc, answer|
      allow(transfer_client).to receive(rpc) { |req| public_send(answer, req.id) }
    end
  end
end
