use crate::{
    config::{GCSPath, Identity},
    gcp_oauth::OauthTokenProvider,
    transport::{Transport, TransportWriter},
    Error,
};
use anyhow::{anyhow, Context, Result};
use log::info;
use std::{
    io,
    io::{Read, Write},
};

const STORAGE_API_BASE_URL: &str = "https://storage.googleapis.com";

/// GCSTransport manages reading and writing from GCS buckets, with
/// authenticatiom to the API by Oauth token in an Authorization header. This
/// struct can either use the default service account from the metadata service,
/// or can impersonate another GCP service account if one is provided to
/// GCSTransport::new.
#[derive(Debug)]
pub struct GCSTransport {
    path: GCSPath,
    oauth_token_provider: OauthTokenProvider,
}

impl GCSTransport {
    /// Instantiate a new GCSTransport to read or write objects from or to the
    /// provided path. If identity is None, GCSTransport authenticates to GCS
    /// as the default service account. If identity contains a service
    /// account email, GCSTransport will use the GCP IAM API to obtain an Oauth
    /// token to impersonate that service account.
    pub fn new(
        path: GCSPath,
        identity: Identity,
        key_file_reader: Option<Box<dyn Read>>,
    ) -> Result<GCSTransport> {
        Ok(GCSTransport {
            path: path.ensure_directory_prefix(),
            oauth_token_provider: OauthTokenProvider::new(
                // This token is used to access GCS storage
                // https://developers.google.com/identity/protocols/oauth2/scopes#storage
                "https://www.googleapis.com/auth/devstorage.read_write",
                identity.map(|x| x.to_string()),
                key_file_reader,
            )?,
        })
    }
}

impl Transport for GCSTransport {
    fn path(&self) -> String {
        self.path.to_string()
    }

    fn get(&mut self, key: &str) -> Result<Box<dyn Read>> {
        info!(
            "get {}/{} as {:?}",
            self.path, key, self.oauth_token_provider
        );
        // Per API reference, the object key must be URL encoded.
        // API reference: https://cloud.google.com/storage/docs/json_api/v1/objects/get
        let encoded_key = urlencoding::encode(&[&self.path.key, key].concat());
        let url = format!(
            "{}/storage/v1/b/{}/o/{}",
            STORAGE_API_BASE_URL, self.path.bucket, encoded_key
        );

        let response = ureq::get(&url)
            // Ensures response body will be content and not JSON metadata.
            // https://cloud.google.com/storage/docs/json_api/v1/objects/get#parameters
            .query("alt", "media")
            .set(
                "Authorization",
                &format!("Bearer {}", self.oauth_token_provider.ensure_oauth_token()?),
            )
            // By default, ureq will wait forever to connect or read
            .timeout_connect(10_000) // ten seconds
            .timeout_read(10_000) // ten seconds
            .call();
        if response.error() {
            return Err(anyhow!(
                "failed to fetch object {} from GCS: {:?}",
                url,
                response
            ));
        }
        Ok(Box::new(response.into_reader()))
    }

    fn put(&mut self, key: &str) -> Result<Box<dyn TransportWriter>> {
        info!(
            "put {}/{} as {:?}",
            self.path, key, self.oauth_token_provider
        );
        // The Oauth token will only be used once, during the call to
        // StreamingTransferWriter::new, so we don't have to worry about it
        // expiring during the lifetime of that object, and so obtain a token
        // here instead of passing the token provider into the
        // StreamingTransferWriter.
        let oauth_token = self.oauth_token_provider.ensure_oauth_token()?;
        let writer = StreamingTransferWriter::new(
            self.path.bucket.to_owned(),
            [&self.path.key, key].concat(),
            oauth_token,
        )?;
        Ok(Box::new(writer))
    }
}

// StreamingTransferWriter implements GCS's resumable, streaming upload feature,
// allowing us to stream data into the GCS buckets.
//
// The GCS resumable, streaming upload API is, frankly, diabolical. The idea is
// that you initiate a transfer with a POST request to an upload endpoint, as in
// StreamingTransferWriter::new_with_api_url, which gets you an upload session
// URI. Then, you perform multiple PUTs to the session URI that each have a
// Content-Range header indicating which chunk of the object they make up. As we
// don't know the final length of the object, we set Content-Range: bytes x-y/*,
// where x and y are the indices of the current slice. We indicate that an
// object upload is finished by setting the final, total size of the object in
// the last field of the Content-Range header in the last PUT request to the
// upload session URI.
// Now, Google mandates that upload chunks be at least 256 KiB, and recommend a
// chunk size of 8 MiB. Where it gets hair raising is that there's no guarantee
// a whole chunk will be uploaded at once: responses to the PUT requests include
// a Range header telling you how much of the chunk Google got, so you can build
// the next PUT request appropriately. So suppose you are trying to upload an 8
// MiB chunk from the middle of the overall object, and you succeed, but Google
// tells you they didn't get the last 100 KiB. You might right away want to
// upload that last 100 KiB, but if you try, you will fail because it's not the
// final chunk and it's less than 256 KiB. So we do two special things in
// upload_chunk when we know it's the last chunk: (1) we construct the Content-
// Range header without any asterisks (2) we drain self.buffer.
struct StreamingTransferWriter {
    upload_session_uri: String,
    minimum_upload_chunk_size: usize,
    object_upload_position: usize,
    buffer: Vec<u8>,
}

impl StreamingTransferWriter {
    /// Creates a new writer that streams content in chunks into GCS. Bucket is
    /// the name of the GCS bucket. Object is the full name of the object being
    /// uploaded, which may contain path separators or file extensions.
    /// oauth_token is used to initiate the initial resumable upload request.
    fn new(bucket: String, object: String, oauth_token: String) -> Result<StreamingTransferWriter> {
        StreamingTransferWriter::new_with_api_url(
            bucket,
            object,
            oauth_token,
            // GCP documentation recommends setting upload part size to 8 MiB.
            // https://cloud.google.com/storage/docs/performing-resumable-uploads#chunked-upload
            8_388_608,
            STORAGE_API_BASE_URL,
        )
    }

    fn new_with_api_url(
        bucket: String,
        object: String,
        oauth_token: String,
        minimum_upload_chunk_size: usize,
        storage_api_base_url: &str,
    ) -> Result<StreamingTransferWriter> {
        // Initiate the resumable, streaming upload.
        // https://cloud.google.com/storage/docs/performing-resumable-uploads#initiate-session
        let encoded_object = urlencoding::encode(&object);
        let upload_url = format!("{}/upload/storage/v1/b/{}/o/", storage_api_base_url, bucket);
        let http_response = ureq::post(&upload_url)
            .set("Authorization", &format!("Bearer {}", oauth_token))
            .query("uploadType", "resumable")
            .query("name", &encoded_object)
            // By default, ureq will wait forever to connect or read
            .timeout_connect(10_000) // ten seconds
            .timeout_read(10_000) // ten seconds
            .send_bytes(&[]);
        if http_response.error() {
            return Err(anyhow!("uploading to gs://{}: {:?}", bucket, http_response));
        }

        // The upload session URI authenticates subsequent upload requests for
        // this upload, so we no longer need the impersonated service account's
        // Oauth token. Session URIs are valid for a week, which should be more
        // than enough for any upload we perform.
        // https://cloud.google.com/storage/docs/resumable-uploads#session-uris
        let upload_session_uri = http_response
            .header("Location")
            .context("no Location header in response when initiating streaming transfer")?;

        Ok(StreamingTransferWriter {
            minimum_upload_chunk_size,
            buffer: Vec::with_capacity(minimum_upload_chunk_size * 2),
            object_upload_position: 0,
            upload_session_uri: upload_session_uri.to_owned(),
        })
    }

    fn upload_chunk(&mut self, last_chunk: bool) -> Result<()> {
        if self.buffer.is_empty() {
            return Ok(());
        }

        if !last_chunk && self.buffer.len() < self.minimum_upload_chunk_size {
            return Err(anyhow!(
                "insufficient content accumulated in buffer to upload chunk"
            ));
        }

        // When this is the last piece being uploaded, the Content-Range header
        // should include the total object size, but otherwise should have * to
        // indicate to GCS that there is an unknown further amount to come.
        // https://cloud.google.com/storage/docs/streaming#streaming_uploads
        let (body, content_range_header_total_length_field) =
            if last_chunk && self.buffer.len() < self.minimum_upload_chunk_size {
                (
                    self.buffer.as_ref(),
                    format!("{}", self.object_upload_position + self.buffer.len()),
                )
            } else {
                (
                    &self.buffer[..self.minimum_upload_chunk_size],
                    "*".to_owned(),
                )
            };

        let content_range = format!(
            "bytes {}-{}/{}",
            self.object_upload_position,
            self.object_upload_position + body.len() - 1,
            content_range_header_total_length_field
        );

        let http_response = ureq::put(&self.upload_session_uri)
            .set("Content-Range", &content_range)
            // By default, ureq will wait forever to connect or read
            .timeout_connect(10_000) // ten seconds
            .timeout_read(10_000) // ten seconds
            .send_bytes(body);

        // On success we expect HTTP 308 Resume Incomplete and a Range: header,
        // unless this is the last part and the server accepts the entire
        // provided Content-Range, in which case it's HTTP 200, or 201 (?).
        // https://cloud.google.com/storage/docs/performing-resumable-uploads#chunked-upload
        match http_response.status() {
            200 | 201 if last_chunk => {
                // Truncate the buffer to "drain" it of uploaded bytes
                self.buffer.truncate(0);
                Ok(())
            }
            200 | 201 => Err(anyhow!(
                "received HTTP 200 or 201 response with chunks remaining"
            )),
            308 if !http_response.has("Range") => Err(anyhow!(
                "No range header in response from GCS: {:?}",
                http_response.into_string()
            )),
            308 => {
                let range_header = http_response.header("Range").unwrap();
                // The range header is like "bytes=0-222", and represents the
                // uploaded portion of the overall object, not the current chunk
                let end = range_header
                    .strip_prefix("bytes=0-")
                    .context(format!(
                        "Range header {} missing bytes prefix",
                        range_header
                    ))?
                    .parse::<usize>()
                    .context("End in range header {} not a valid usize")?;
                // end is usize and so parse would fail if the value in the
                // header was negative, but we still defend ourselves against
                // it being less than it was before this chunk was uploaded, or
                // being bigger than is possible given our position in the
                // overall object.
                if end < self.object_upload_position
                    || end > self.object_upload_position + body.len() - 1
                {
                    return Err(anyhow!("End in range header {} is invalid", range_header));
                }

                // If we have a little content left over, we can't just make
                // another request, because if there's too little of it, Google
                // will reject it. Instead, leave the portion of the chunk that
                // we didn't manage to upload back in self.buffer so it can be
                // handled by a subsequent call to upload_chunk.
                self.buffer = self.buffer.split_off(end + 1 - self.object_upload_position);
                self.object_upload_position = end + 1;
                Ok(())
            }
            _ => Err(anyhow!(
                "failed to upload part to GCS: {} synthetic: {}\n{:?}",
                http_response.status(),
                http_response.synthetic(),
                http_response.into_string()
            )),
        }
    }
}

impl Write for StreamingTransferWriter {
    fn write(&mut self, buf: &[u8]) -> io::Result<usize> {
        // Write into memory buffer, and upload to GCS if we have accumulated
        // enough content
        self.buffer.extend_from_slice(buf);
        while self.buffer.len() >= self.minimum_upload_chunk_size {
            self.upload_chunk(false)
                .map_err(|e| io::Error::new(io::ErrorKind::Other, Error::AnyhowError(e)))?;
        }

        Ok(buf.len())
    }

    fn flush(&mut self) -> io::Result<()> {
        // It may not be possible to flush this if we have accumulated less
        // content in the buffer than Google will allow us to upload, and users
        // of a TransportWriter are expected to call complete_upload when they
        // know they are finished anyway, so we just report success.
        Ok(())
    }
}

impl TransportWriter for StreamingTransferWriter {
    fn complete_upload(&mut self) -> Result<()> {
        while !self.buffer.is_empty() {
            self.upload_chunk(true)?;
        }
        Ok(())
    }

    fn cancel_upload(&mut self) -> Result<()> {
        // https://cloud.google.com/storage/docs/performing-resumable-uploads#cancel-upload
        let http_response = ureq::delete(&self.upload_session_uri)
            .set("Content-Length", "0")
            // By default, ureq will wait forever to connect or read
            .timeout_connect(10_000) // ten seconds
            .timeout_read(10_000) // ten seconds
            .call();
        match http_response.status() {
            499 => Ok(()),
            _ => Err(anyhow!(
                "failed to cancel streaming transfer to GCS: {:?}",
                http_response
            )),
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use mockito::{mock, Matcher};

    #[test]
    fn simple_upload() {
        let fake_upload_session_uri = format!("{}/fake-session-uri", mockito::server_url());
        let mocked_post = mock("POST", "/upload/storage/v1/b/fake-bucket/o/")
            .match_header("Authorization", "Bearer fake-token")
            .match_header("Content-Length", "0")
            .match_query(Matcher::UrlEncoded(
                "uploadType".to_owned(),
                "resumable".to_owned(),
            ))
            .match_query(Matcher::UrlEncoded(
                "name".to_owned(),
                "fake-object".to_owned(),
            ))
            .with_status(200)
            .with_header("Location", &fake_upload_session_uri)
            .expect_at_most(1)
            .create();

        let mut writer = StreamingTransferWriter::new_with_api_url(
            "fake-bucket".to_string(),
            "fake-object".to_string(),
            "fake-token".to_string(),
            10,
            &mockito::server_url(),
        )
        .unwrap();

        mocked_post.assert();

        let mocked_put = mock("PUT", "/fake-session-uri")
            .match_header("Content-Length", "7")
            .match_header("Content-Range", "bytes 0-6/7")
            .match_body("content")
            .with_status(200)
            .expect_at_most(1)
            .create();

        assert_eq!(writer.write(b"content").unwrap(), 7);
        writer.complete_upload().unwrap();

        mocked_put.assert();
    }

    #[test]
    fn multi_chunk_upload() {
        let fake_upload_session_uri = format!("{}/fake-session-uri", mockito::server_url());
        let mocked_post = mock("POST", "/upload/storage/v1/b/fake-bucket/o/")
            .match_header("Authorization", "Bearer fake-token")
            .match_header("Content-Length", "0")
            .match_query(Matcher::UrlEncoded(
                "uploadType".to_owned(),
                "resumable".to_owned(),
            ))
            .match_query(Matcher::UrlEncoded(
                "name".to_owned(),
                "fake-object".to_owned(),
            ))
            .with_status(200)
            .with_header("Location", &fake_upload_session_uri)
            .expect_at_most(1)
            .create();

        let mut writer = StreamingTransferWriter::new_with_api_url(
            "fake-bucket".to_string(),
            "fake-object".to_string(),
            "fake-token".to_string(),
            4,
            &mockito::server_url(),
        )
        .unwrap();

        mocked_post.assert();

        let first_mocked_put = mock("PUT", "/fake-session-uri")
            .match_header("Content-Length", "4")
            .match_header("Content-Range", "bytes 0-3/*")
            .match_body("0123")
            .with_status(308)
            .with_header("Range", "bytes=0-3")
            .expect_at_most(1)
            .create();

        let second_mocked_put = mock("PUT", "/fake-session-uri")
            .match_header("Content-Length", "4")
            .match_header("Content-Range", "bytes 4-7/*")
            .match_body("4567")
            .with_status(308)
            .with_header("Range", "bytes=0-6")
            .expect_at_most(1)
            .create();

        let final_mocked_put = mock("PUT", "/fake-session-uri")
            .match_header("Content-Length", "3")
            .match_header("Content-Range", "bytes 7-9/10")
            .match_body("789")
            .with_status(200)
            .expect_at_most(1)
            .create();

        assert_eq!(writer.write(b"0123456789").unwrap(), 10);
        writer.complete_upload().unwrap();

        first_mocked_put.assert();
        second_mocked_put.assert();
        final_mocked_put.assert();
    }
}
