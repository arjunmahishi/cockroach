// Copyright 2020 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

import React from "react";

import { Anchor } from "src/components";
import * as docsURL from "src/util/docs";

export const CapacityGraphTooltip: React.FC<{ tooltipSelection?: string }> = ({
  tooltipSelection,
}) => (
  <div>
    <dl>
      <dd>
        <p>{`Usage of disk space ${tooltipSelection}`}</p>
        <p>
          <strong>Capacity: </strong>
          Maximum store size{" "}
          {tooltipSelection || "across all nodes / on node <node>"}. This value
          may be explicitly set per node using&nbsp;
          <Anchor href={docsURL.clusterStore} className={"anchor-light"}>
            --store
          </Anchor>
          . If a store size has not been set, this metric displays the actual
          disk capacity.
        </p>
        <p>
          <strong>Available: </strong>
          Free disk space available to CockroachDB data{" "}
          {tooltipSelection || "across all nodes / on node <node>"}.
        </p>
        <p>
          <strong>Used: </strong>
          Disk space in use by CockroachDB data{" "}
          {tooltipSelection || "across all nodes / on node <node>"}. This
          excludes the Cockroach binary, operating system, and other system
          files.
        </p>
        <p>
          <Anchor
            href={docsURL.howAreCapacityMetricsCalculated}
            className={"anchor-light"}
          >
            How are these metrics calculated?
          </Anchor>
        </p>
      </dd>
    </dl>
  </div>
);

export const AvailableDiscCapacityGraphTooltip: React.FC<{}> = () => (
  <div>
    <dl>
      <dd>
        <p>Free disk space available to CockroachDB data on each node.</p>
        <p>
          <Anchor
            href={docsURL.howAreCapacityMetricsCalculated}
            className={"anchor-light"}
          >
            How is this metric calculated?
          </Anchor>
        </p>
      </dd>
    </dl>
  </div>
);

export const LogicalBytesGraphTooltip: React.FC = () => (
  <div>
    <dl>
      <dd>
        <p>
          {"Number of logical bytes stored in "}
          <Anchor href={docsURL.keyValuePairs} className={"anchor-light"}>
            key-value pairs
          </Anchor>
          {" on each node."}
        </p>
        <p>This includes historical and deleted data.</p>
      </dd>
    </dl>
  </div>
);

export const LiveBytesGraphTooltip: React.FC<{ tooltipSelection?: string }> = ({
  tooltipSelection,
}) => (
  <div>
    <dl>
      <dd>
        <p>
          Amount of data that can be read by applications and CockroachDB{" "}
          {tooltipSelection}.
        </p>
        <p>
          <strong>Live: </strong>
          Number of logical bytes stored in live&nbsp;
          <Anchor href={docsURL.keyValuePairs} className={"anchor-light"}>
            key-value pairs&nbsp;
          </Anchor>
          {tooltipSelection || "across all nodes / on node <node>"}. Live data
          excludes historical and deleted data.
        </p>
        <p>
          <strong>System: </strong>
          Number of physical bytes stored in&nbsp;
          <Anchor href={docsURL.keyValuePairs} className={"anchor-light"}>
            system key-value pairs&nbsp;
          </Anchor>
          {tooltipSelection || "across all nodes / on node <node>"}.
        </p>
      </dd>
    </dl>
  </div>
);

export const StatementDenialsClusterSettingsTooltip: React.FC<{
  tooltipSelection?: string;
}> = ({ tooltipSelection }) => (
  <div>
    The total number of statements denied per second {tooltipSelection} due to a
    <Anchor href={docsURL.clusterSettings} className={"anchor-light"}>
      {" "}
      cluster setting{" "}
    </Anchor>
    in the format feature.statement_type.enabled = FALSE.
  </div>
);

export const TransactionRestartsToolTip: React.FC<{
  tooltipSelection?: string;
}> = ({ tooltipSelection }) => (
  <div>
    The number of transactions restarted broken down by errors{" "}
    {tooltipSelection}. Refer to the transaction retry error reference{" "}
    <Anchor
      href={docsURL.transactionRetryErrorReference}
      className={"anchor-light"}
    >
      documentation
    </Anchor>{" "}
    for more details.
  </div>
);

export const CircuitBreakerTrippedReplicasTooltip: React.ReactNode = (
  <div>
    Number of Replicas for which the per-Replica circuit breaker is currently
    tripped.
  </div>
);

export const CircuitBreakerTrippedEventsTooltip: React.ReactNode = (
  <div>
    The number of circuit breaker events occurred per aggregated interval of
    time across all nodes since the process started.
  </div>
);

export const ReceiverSnapshotsQueuedTooltip: React.ReactNode = (
  <div>
    The number of snapshots queued to be applied on a receiver which can only{" "}
    accept 1 at a time per store.
  </div>
);
