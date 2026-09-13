# Send and CCTP

Choose **Send** beside a token's network row. Enter an address or client-resolved
name and an exact decimal amount, then connect the injected EIP-1193 wallet to
review the recipient, token, chain, estimated gas and calldata. Only the account
that owns the displayed holding can send. The wallet confirms every transaction.
ERC-20 `transfer(address,uint256)` is the basic send operation; a successful token
contract emits the Transfer event. NFT and native-coin sends are outside this version.

For native USDC, select a CCTP destination. This version supports Ethereum, Base,
Arbitrum and OP Mainnet, and separately Ethereum Sepolia ↔ Base Sepolia. Routes
match chain ID and Circle's token address, never a symbol or a stablecoin label.
Bridged USDC, USDT, DAI and other tokens retain same-network transfers only.

The steps are exact-amount approval if needed, review again, burn, check Circle's
attestation, and review/confirm the mint on the destination chain. Standard
finality (2000) is requested. The fee cap is entered in USDC, defaults to zero,
and must be below the amount. A route requiring a higher cap will fail simulation;
review its current fee before raising the cap. No fee is silently increased.
There is no forwarding service: the wallet pays native gas on both chains.

The burn hash and source network are saved as `evmscan.cctp.last`. The recovery
panel also accepts them manually, without requiring a portfolio lookup. Attestation
checks are explicit, so long waits don't leave a browser polling indefinitely.
The source receipt's MessageSent is compared to Iris's message, allowing Circle's
assigned nonce, finality, fee and expiry fields to change. Transfer identity must
match. Destination simulation and MessageTransmitter validate the signature and
replay status. Already-minted messages cannot mint twice. If a transaction is
pending or its receipt is unavailable, the current action retains its hash and
only offers a receipt check. Reloaded ordinary sends must be checked in the wallet
before sending again. Replaced/cancelled transactions may need the wallet's new hash.

The browser talks directly to the injected wallet and Circle's Iris API. The daemon
holds no keys, relays no sends, and receives no send requests. Circle's issuer and
attester remain trusted: freezing, pausing and unavailable attestations can delay
completion. Iris learns the IP and burn hash. There is no alternative attester if
Circle is unavailable; the saved hash lets another CCTP client resume later.

Implementation: `web/send.mjs`; no build step or contract deployment. Route and ABI
references checked against Circle's documentation on 2026-09-13:

- [CCTP contracts](https://developers.circle.com/cctp/references/contract-addresses)
- [Native USDC addresses](https://developers.circle.com/stablecoins/usdc-contract-addresses)
- [Interfaces](https://developers.circle.com/cctp/references/contract-interfaces)
- [Message layout and API](https://developers.circle.com/cctp/references/technical-guide)
- [Finality](https://developers.circle.com/cctp/concepts/finality-and-block-confirmations)

Validation: `npm test --prefix web/tests` and `node web/tests/send-browser.mjs`.
Set `PLAYWRIGHT_CHROMIUM_EXECUTABLE` if using an installed Chrome instead of
Playwright's bundled Chromium. Browser tests mock the wallet and Iris; they do
not spend funds. Live Sepolia end-to-end validation remains required before release.
