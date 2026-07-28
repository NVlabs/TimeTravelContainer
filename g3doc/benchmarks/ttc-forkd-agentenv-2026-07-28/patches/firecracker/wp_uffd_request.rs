// Reconstructed for benchmarking from deeplethe/firecracker commit
// 7d80afade831f01e3178c5b76daf8b4cbc02d176, whose commit message and routing
// changes reference this file but whose Git tree omits it.

use vmm::rpc_interface::VmmAction;
use vmm::vmm_config::wp_uffd::SetupWpUffdParams;

use super::super::parsed_request::{ParsedRequest, RequestError};
use super::super::request::{Body, Method, StatusCode};

pub(crate) fn parse_put_uffd(
    body: &Body,
    request_type_from_path: Option<&str>,
) -> Result<ParsedRequest, RequestError> {
    match request_type_from_path {
        Some("wp") => {
            let params = serde_json::from_slice::<SetupWpUffdParams>(body.raw())?;
            Ok(ParsedRequest::new_sync(VmmAction::SetupWpUffd(params)))
        }
        Some(request_type) => Err(RequestError::InvalidPathMethod(
            format!("/uffd/{request_type}"),
            Method::Put,
        )),
        None => Err(RequestError::Generic(
            StatusCode::BadRequest,
            "Missing uffd operation type.".to_string(),
        )),
    }
}
