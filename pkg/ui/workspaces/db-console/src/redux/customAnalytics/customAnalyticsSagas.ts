// Copyright 2020 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

import Analytics from "analytics-node";
import { call, put, takeEvery } from "redux-saga/effects";

import { PayloadAction } from "src/interfaces/action";
import { emailSubscriptionAlertLocalSetting } from "src/redux/alerts";
import { COCKROACHLABS_ADDR } from "src/util/cockroachlabsAPI";

import {
  EMAIL_SUBSCRIPTION_SIGN_UP,
  EmailSubscriptionSignUpPayload,
} from "./customAnanlyticsActions";

export type AnalyticsClientTarget = "email_sign_up";

// TODO (koorosh): has to be moved out from code base
const EMAIL_SIGN_UP_CLIENT_KEY = "72EEC0nqQKfoLWq0ZcGoTkJFIG9G9SII";

const analyticsOpts = {
  host: COCKROACHLABS_ADDR + "/api/segment",
};

export function getAnalyticsClientFor(
  target: AnalyticsClientTarget,
): Analytics {
  switch (target) {
    case "email_sign_up":
      return new Analytics(EMAIL_SIGN_UP_CLIENT_KEY, analyticsOpts);
    default:
      throw new Error("Unrecognized Analytics Client target.");
  }
}

export function* signUpEmailSubscription(
  action: PayloadAction<EmailSubscriptionSignUpPayload>,
) {
  const client = getAnalyticsClientFor("email_sign_up");
  const { clusterId, email } = action.payload;
  yield call([client, client.identify], {
    userId: clusterId,
    traits: {
      email,
      release_notes_sign_up_from_admin_ui: "true",
      product_updates: "true",
    },
  });
  yield put(emailSubscriptionAlertLocalSetting.set(true));
}

export function* customAnalyticsSaga() {
  yield takeEvery(EMAIL_SUBSCRIPTION_SIGN_UP, signUpEmailSubscription);
}
