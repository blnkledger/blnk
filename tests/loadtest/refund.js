import http from "k6/http";
import { uuidv4 } from "https://jslib.k6.io/k6-utils/1.4.0/index.js";
import { check, sleep } from "k6";

export const options = {
  vus: 5,
  duration: "10s",
};

export default function () {
  const url = "http://localhost:5001/transactions";
  const refundUrl = "http://localhost:5001/refund-transaction/";

  // Step 1: Create and commit a transaction
  let payload = JSON.stringify({
    amount: 500,
    description: "test transaction",
    precision: 100,
    allow_overdraft: true,
    reference: uuidv4(),
    currency: "USD",
    source: `@world`,
    destination: `@${uuidv4()}`,
  });

  let params = {
    headers: {
      "Content-Type": "application/json",
    },
  };

  let response = http.post(url, payload, params);
  check(response, {
    "is status 201": (r) => r.status === 201,
  });

  let transactionId = JSON.parse(response.body).transaction_id;

  // Step 2: Refund the transaction
  response = http.post(`${refundUrl}${transactionId}`, null, params);
  check(response, {
    "is status 201": (r) => r.status === 201,
  });
}
