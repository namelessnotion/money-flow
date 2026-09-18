# Money Flow client

Vue 3 and TypeScript client for the Ruby GraphQL API. Apollo Client sends
requests to `/graphql`.

Start the Ruby server first:

```sh
cd ../ruby
bin/server
```

Then run the client:

```sh
npm install
npm run dev
```

Vite proxies `/graphql` to `http://localhost:9292` by default. Set
`VITE_RUBY_SERVER_URL` to change the development proxy target, or
`VITE_GRAPHQL_URL` to make Apollo Client use an explicit GraphQL URL.

Routes (`src/router.ts`):

- `/` — onboard and list entities
- `/entities/:entityId` — an entity's accounts, ACH deposit / withdrawal, and its ACH transactions
- `/entities/:entityId/ach-transactions/:id` — one ACH transaction and its lifecycle steps

Views poll every 5 seconds, because transaction state comes from a read model that catches up with the
ledger in the background.

Run tests with:

```sh
npm test
```
