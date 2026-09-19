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

  def go_gateway
    Services::Ach::GoGateway.new(transaction_client: transaction_client, transfer_client: transfer_client)
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

  def resumed(id, state)
    twirp_ok(Transaction::V1::ResumeTransactionResponse.new(id: id, state: state))
  end

  # Stubs every Go RPC an ACH Transaction's life touches with its happy-path
  # answer, echoing the id each request was sent with.
  def stub_go_happy_path
    stub_transaction_service_happy_path
    stub_transfer_service_happy_path
  end

  def stub_transaction_service_happy_path
    allow(transaction_client).to receive(:start_initializing_transaction) { |req| transaction_initialized(req.id) }
    allow(transaction_client).to receive(:resume_transaction) { |req| resumed(req.id, :TRANSACTION_STATE_STARTED) }
  end

  def stub_transfer_service_happy_path
    { confirm_staged_transfer: :transfer_pending,
      post_pending_transfer: :transfer_committed,
      cancel_staged_transfer: :transfer_cancelled }.each do |rpc, answer|
      allow(transfer_client).to receive(rpc) { |req| public_send(answer, req.id) }
    end
  end
end
