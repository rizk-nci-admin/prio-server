use anyhow::{anyhow, Context, Result};
use derivative::Derivative;
use log::info;
use rusoto_core::Region;
use rusoto_sqs::{
    ChangeMessageVisibilityRequest, DeleteMessageRequest, ReceiveMessageRequest, Sqs, SqsClient,
};
use std::{marker::PhantomData, str::FromStr};
use tokio::runtime::Runtime;

use crate::{
    aws_credentials::{basic_runtime, DefaultCredentialsProvider},
    task::{Task, TaskHandle, TaskQueue},
};

/// A task queue backed by AWS SQS
#[derive(Derivative)]
#[derivative(Debug)]
pub struct AwsSqsTaskQueue<T: Task> {
    #[derivative(Debug = "ignore")]
    client: SqsClient,
    queue_url: String,
    runtime: Runtime,
    phantom_task: PhantomData<*const T>,
}

impl<T: Task> AwsSqsTaskQueue<T> {
    pub fn new(region: &str, queue_url: &str) -> Result<AwsSqsTaskQueue<T>> {
        let region = Region::from_str(region).context("invalid AWS region")?;
        let runtime = basic_runtime()?;

        // Credentials for authenticating to AWS are automatically
        // sourced from environment variables or ~/.aws/credentials.
        // https://github.com/rusoto/rusoto/blob/master/AWS-CREDENTIALS.md
        let credentials_provider =
            DefaultCredentialsProvider::new().context("failed to create credentials provider")?;

        let http_client = rusoto_core::HttpClient::new().context("failed to create HTTP client")?;

        Ok(AwsSqsTaskQueue {
            client: SqsClient::new_with(http_client, credentials_provider, region),
            queue_url: queue_url.to_owned(),
            runtime: runtime,
            phantom_task: PhantomData,
        })
    }
}

impl<T: Task> TaskQueue<T> for AwsSqsTaskQueue<T> {
    fn dequeue(&mut self) -> Result<Option<TaskHandle<T>>> {
        info!("pull task from {}", self.queue_url);

        let request = ReceiveMessageRequest {
            // Dequeue one task at a time
            max_number_of_messages: Some(1),
            queue_url: self.queue_url.clone(),
            // Long polling. SQS allows us to wait up to 20 seconds.
            // https://docs.aws.amazon.com/AWSSimpleQueueService/latest/SQSDeveloperGuide/sqs-short-and-long-polling.html#sqs-long-polling
            wait_time_seconds: Some(20),
            // Visibility timeout configures how long SQS will wait for message
            // deletion by this client before making a message visible again to
            // other queue consumers. We set it to 600s = 10 minutes.
            visibility_timeout: Some(600),
            ..Default::default()
        };

        let response = self
            .runtime
            .block_on(self.client.receive_message(request))
            .context("failed to dequeue message from SQS")?;

        let received_messages = match response.messages {
            Some(ref messages) => messages,
            None => return Ok(None),
        };

        if received_messages.len() == 0 {
            return Ok(None);
        }

        if received_messages.len() > 1 {
            return Err(anyhow!(
                "unexpected number of messages in SQS response: {:?}",
                response
            ));
        }

        let body = match &received_messages[0].body {
            Some(body) => body,
            None => return Err(anyhow!("no body in SQS message")),
        };
        let receipt_handle = match &received_messages[0].receipt_handle {
            Some(handle) => handle,
            None => return Err(anyhow!("no receipt handle in SQS message")),
        };

        let task = serde_json::from_reader(body.as_bytes())
            .context(format!("failed to decode JSON task {:?}", body))?;

        Ok(Some(TaskHandle {
            task: task,
            acknowledgment_id: receipt_handle.to_owned(),
        }))
    }

    fn acknowledge_task(&mut self, task: TaskHandle<T>) -> Result<()> {
        info!(
            "acknowledging task {} in queue {}",
            task.acknowledgment_id, self.queue_url
        );

        let request = DeleteMessageRequest {
            queue_url: self.queue_url.clone(),
            receipt_handle: task.acknowledgment_id.clone(),
        };

        Ok(self
            .runtime
            .block_on(self.client.delete_message(request))
            .context("failed to delete/acknowledge message in SQS")?)
    }

    fn nacknowledge_task(&mut self, task: TaskHandle<T>) -> Result<()> {
        // In SQS, messages are nacked by changing the message visibility
        // timeout to 0
        // https://docs.aws.amazon.com/AWSSimpleQueueService/latest/SQSDeveloperGuide/sqs-visibility-timeout.html#terminating-message-visibility-timeout
        info!(
            "nacknowledging task {} in queue {}",
            task.acknowledgment_id, self.queue_url
        );

        let request = ChangeMessageVisibilityRequest {
            queue_url: self.queue_url.clone(),
            receipt_handle: task.acknowledgment_id.clone(),
            visibility_timeout: 0,
        };

        Ok(self
            .runtime
            .block_on(self.client.change_message_visibility(request))
            .context("failed to change message visibility/nacknowledge message in SQS")?)
    }
}
