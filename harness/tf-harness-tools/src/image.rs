//! Port of pi's image handling for the read tool: MIME sniffing
//! (`utils/mime.ts`) and the normalize/resize pipeline
//! (`utils/image-process.ts`, `utils/image-resize-core.ts`).
//!
//! Pixel output uses the `image` crate where pi uses Photon (also
//! Lanczos3), so encoded bytes differ; every model-visible string and the
//! decision logic (passthrough limits, PNG-vs-JPEG candidates, quality
//! ladder, 0.75 shrink loop) match pi.

use base64::Engine;
use image::imageops::FilterType;
use image::DynamicImage;

pub const IMAGE_TYPE_SNIFF_BYTES: usize = 4100;
const PNG_SIGNATURE: [u8; 8] = [0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a];

const DEFAULT_MAX_WIDTH: u32 = 2000;
const DEFAULT_MAX_HEIGHT: u32 = 2000;
/// 4.5MB of base64 payload — headroom below Anthropic's 5MB limit.
const DEFAULT_MAX_BYTES: f64 = 4.5 * 1024.0 * 1024.0;
const DEFAULT_JPEG_QUALITY: u8 = 80;

fn starts_with_ascii(buffer: &[u8], offset: usize, text: &str) -> bool {
    buffer.len() >= offset + text.len() && &buffer[offset..offset + text.len()] == text.as_bytes()
}

fn read_u32_be(buffer: &[u8], offset: usize) -> u32 {
    let get = |i: usize| u32::from(buffer.get(i).copied().unwrap_or(0));
    get(offset) * 0x0100_0000 + (get(offset + 1) << 16) + (get(offset + 2) << 8) + get(offset + 3)
}

fn read_u32_le(buffer: &[u8], offset: usize) -> u32 {
    let get = |i: usize| u32::from(buffer.get(i).copied().unwrap_or(0));
    get(offset) + (get(offset + 1) << 8) + (get(offset + 2) << 16) + get(offset + 3) * 0x0100_0000
}

fn read_u16_le(buffer: &[u8], offset: usize) -> u16 {
    let get = |i: usize| u16::from(buffer.get(i).copied().unwrap_or(0));
    get(offset) + (get(offset + 1) << 8)
}

fn is_png(buffer: &[u8]) -> bool {
    buffer.len() >= 16
        && read_u32_be(buffer, PNG_SIGNATURE.len()) == 13
        && starts_with_ascii(buffer, 12, "IHDR")
}

fn is_animated_png(buffer: &[u8]) -> bool {
    let mut offset = PNG_SIGNATURE.len();
    while offset + 8 <= buffer.len() {
        let chunk_length = read_u32_be(buffer, offset) as usize;
        let chunk_type_offset = offset + 4;
        if starts_with_ascii(buffer, chunk_type_offset, "acTL") {
            return true;
        }
        if starts_with_ascii(buffer, chunk_type_offset, "IDAT") {
            return false;
        }
        let next_offset = offset + 8 + chunk_length + 4;
        if next_offset <= offset || next_offset > buffer.len() {
            return false;
        }
        offset = next_offset;
    }
    false
}

fn is_bmp(buffer: &[u8]) -> bool {
    if buffer.len() < 26 {
        return false;
    }
    let declared_file_size = read_u32_le(buffer, 2);
    let pixel_data_offset = read_u32_le(buffer, 10);
    let dib_header_size = read_u32_le(buffer, 14);
    if declared_file_size != 0 && declared_file_size < 26 {
        return false;
    }
    if pixel_data_offset < 14 + dib_header_size {
        return false;
    }
    if declared_file_size != 0 && pixel_data_offset >= declared_file_size {
        return false;
    }
    let (color_planes, bits_per_pixel) = if dib_header_size == 12 {
        (read_u16_le(buffer, 22), read_u16_le(buffer, 24))
    } else if (40..=124).contains(&dib_header_size) {
        if buffer.len() < 30 {
            return false;
        }
        (read_u16_le(buffer, 26), read_u16_le(buffer, 28))
    } else {
        return false;
    };
    color_planes == 1 && [1, 4, 8, 16, 24, 32].contains(&bits_per_pixel)
}

/// pi's `detectSupportedImageMimeType`: magic-based, not extension-based.
pub fn detect_supported_image_mime_type(buffer: &[u8]) -> Option<&'static str> {
    if buffer.len() >= 3 && buffer[0] == 0xff && buffer[1] == 0xd8 && buffer[2] == 0xff {
        return if buffer.get(3) == Some(&0xf7) {
            None
        } else {
            Some("image/jpeg")
        };
    }
    if buffer.len() >= PNG_SIGNATURE.len() && buffer[..8] == PNG_SIGNATURE {
        return if is_png(buffer) && !is_animated_png(buffer) {
            Some("image/png")
        } else {
            None
        };
    }
    if starts_with_ascii(buffer, 0, "GIF") {
        return Some("image/gif");
    }
    if starts_with_ascii(buffer, 0, "RIFF") && starts_with_ascii(buffer, 8, "WEBP") {
        return Some("image/webp");
    }
    if starts_with_ascii(buffer, 0, "BM") && is_bmp(buffer) {
        return Some("image/bmp");
    }
    None
}

pub fn detect_supported_image_mime_type_from_file(
    file_path: &str,
) -> std::io::Result<Option<&'static str>> {
    use std::io::Read;
    let mut file = std::fs::File::open(file_path)?;
    let mut buffer = vec![0u8; IMAGE_TYPE_SNIFF_BYTES];
    let mut filled = 0usize;
    loop {
        let n = file.read(&mut buffer[filled..])?;
        if n == 0 {
            break;
        }
        filled += n;
        if filled == buffer.len() {
            break;
        }
    }
    Ok(detect_supported_image_mime_type(&buffer[..filled]))
}

pub enum ProcessImageResult {
    Ok {
        data: String,
        mime_type: String,
        hints: Vec<String>,
    },
    Err {
        message: String,
    },
}

fn base_mime_type(mime_type: &str) -> String {
    mime_type
        .split(';')
        .next()
        .unwrap_or(mime_type)
        .trim()
        .to_lowercase()
}

fn normalize_supported_image_mime_type(mime_type: &str) -> Option<&'static str> {
    match base_mime_type(mime_type).as_str() {
        "image/png" => Some("image/png"),
        "image/jpeg" | "image/jpg" => Some("image/jpeg"),
        "image/gif" => Some("image/gif"),
        "image/webp" => Some("image/webp"),
        _ => None,
    }
}

fn exif_orientation(bytes: &[u8]) -> u32 {
    let mut cursor = std::io::Cursor::new(bytes);
    match exif::Reader::new().read_from_container(&mut cursor) {
        Ok(data) => data
            .get_field(exif::Tag::Orientation, exif::In::PRIMARY)
            .and_then(|f| f.value.get_uint(0))
            .unwrap_or(1),
        Err(_) => 1,
    }
}

fn apply_exif_orientation(img: DynamicImage, orientation: u32) -> DynamicImage {
    match orientation {
        2 => img.fliph(),
        3 => img.rotate180(),
        4 => img.flipv(),
        5 => img.rotate90().fliph(),
        6 => img.rotate90(),
        7 => img.rotate270().fliph(),
        8 => img.rotate270(),
        _ => img,
    }
}

fn decode_oriented(bytes: &[u8]) -> Option<DynamicImage> {
    let img = image::load_from_memory(bytes).ok()?;
    Some(apply_exif_orientation(img, exif_orientation(bytes)))
}

fn encode_png(img: &DynamicImage) -> Option<Vec<u8>> {
    let mut out = std::io::Cursor::new(Vec::new());
    img.write_to(&mut out, image::ImageFormat::Png).ok()?;
    Some(out.into_inner())
}

fn encode_jpeg(img: &DynamicImage, quality: u8) -> Option<Vec<u8>> {
    let mut out = Vec::new();
    let mut encoder = image::codecs::jpeg::JpegEncoder::new_with_quality(&mut out, quality);
    // JPEG has no alpha; flatten like Photon's JPEG export.
    encoder.encode_image(&img.to_rgb8()).ok()?;
    Some(out)
}

struct Resized {
    data: String,
    mime_type: String,
    original_width: u32,
    original_height: u32,
    width: u32,
    height: u32,
    was_resized: bool,
}

fn b64(bytes: &[u8]) -> String {
    base64::engine::general_purpose::STANDARD.encode(bytes)
}

fn resize_image(input_bytes: &[u8], mime_type: &str) -> Option<Resized> {
    let input_base64_size = input_bytes.len().div_ceil(3) * 4;
    let image = decode_oriented(input_bytes)?;
    let original_width = image.width();
    let original_height = image.height();

    if original_width <= DEFAULT_MAX_WIDTH
        && original_height <= DEFAULT_MAX_HEIGHT
        && (input_base64_size as f64) < DEFAULT_MAX_BYTES
    {
        return Some(Resized {
            data: b64(input_bytes),
            mime_type: mime_type.to_string(),
            original_width,
            original_height,
            width: original_width,
            height: original_height,
            was_resized: false,
        });
    }

    let mut target_width = original_width;
    let mut target_height = original_height;
    if target_width > DEFAULT_MAX_WIDTH {
        target_height = ((f64::from(target_height) * f64::from(DEFAULT_MAX_WIDTH))
            / f64::from(target_width))
        .round() as u32;
        target_width = DEFAULT_MAX_WIDTH;
    }
    if target_height > DEFAULT_MAX_HEIGHT {
        target_width = ((f64::from(target_width) * f64::from(DEFAULT_MAX_HEIGHT))
            / f64::from(target_height))
        .round() as u32;
        target_height = DEFAULT_MAX_HEIGHT;
    }

    let mut quality_steps: Vec<u8> = vec![DEFAULT_JPEG_QUALITY, 85, 70, 55, 40];
    quality_steps.dedup();
    let mut current_width = target_width.max(1);
    let mut current_height = target_height.max(1);

    loop {
        let resized = image.resize_exact(current_width, current_height, FilterType::Lanczos3);
        let mut candidates: Vec<(Vec<u8>, &'static str)> = Vec::new();
        if let Some(png) = encode_png(&resized) {
            candidates.push((png, "image/png"));
        }
        for quality in &quality_steps {
            if let Some(jpeg) = encode_jpeg(&resized, *quality) {
                candidates.push((jpeg, "image/jpeg"));
            }
        }
        for (bytes, mime) in candidates {
            let data = b64(&bytes);
            if (data.len() as f64) < DEFAULT_MAX_BYTES {
                return Some(Resized {
                    data,
                    mime_type: mime.to_string(),
                    original_width,
                    original_height,
                    width: current_width,
                    height: current_height,
                    was_resized: true,
                });
            }
        }

        if current_width == 1 && current_height == 1 {
            return None;
        }
        let next_width = if current_width == 1 {
            1
        } else {
            ((f64::from(current_width) * 0.75).floor() as u32).max(1)
        };
        let next_height = if current_height == 1 {
            1
        } else {
            ((f64::from(current_height) * 0.75).floor() as u32).max(1)
        };
        if next_width == current_width && next_height == current_height {
            return None;
        }
        current_width = next_width;
        current_height = next_height;
    }
}

fn format_dimension_note(result: &Resized) -> Option<String> {
    if !result.was_resized {
        return None;
    }
    let scale = f64::from(result.original_width) / f64::from(result.width);
    Some(format!(
        "[Image: original {}x{}, displayed at {}x{}. Multiply coordinates by {:.2} to map to original image.]",
        result.original_width, result.original_height, result.width, result.height, scale
    ))
}

/// pi's `processImage`.
pub fn process_image(
    bytes: &[u8],
    mime_type: &str,
    auto_resize_images: bool,
) -> ProcessImageResult {
    // Normalize: pass through natively-supported formats; convert the rest
    // (BMP) to PNG.
    let (normalized_bytes, normalized_mime, converted_from): (
        Vec<u8>,
        &'static str,
        Option<String>,
    ) = match normalize_supported_image_mime_type(mime_type) {
        Some(mime) => (bytes.to_vec(), mime, None),
        None => match decode_oriented(bytes).and_then(|img| encode_png(&img)) {
            Some(png) => (png, "image/png", Some(base_mime_type(mime_type))),
            None => return ProcessImageResult::Err {
                message:
                    "[Image omitted: could not be converted to a supported inline image format.]"
                        .to_string(),
            },
        },
    };

    let conversion_hint = |to: &str| -> Option<String> {
        match &converted_from {
            Some(from) if from != to => Some(format!("[Image converted from {from} to {to}.]")),
            _ => None,
        }
    };

    if auto_resize_images {
        let Some(resized) = resize_image(&normalized_bytes, normalized_mime) else {
            return ProcessImageResult::Err {
                message: "[Image omitted: could not be resized below the inline image size limit.]"
                    .to_string(),
            };
        };
        let mut hints: Vec<String> = Vec::new();
        if let Some(hint) = conversion_hint(&resized.mime_type) {
            hints.push(hint);
        }
        if let Some(note) = format_dimension_note(&resized) {
            hints.push(note);
        }
        return ProcessImageResult::Ok {
            data: resized.data,
            mime_type: resized.mime_type,
            hints,
        };
    }

    let mut hints: Vec<String> = Vec::new();
    if let Some(hint) = conversion_hint(normalized_mime) {
        hints.push(hint);
    }
    ProcessImageResult::Ok {
        data: b64(&normalized_bytes),
        mime_type: normalized_mime.to_string(),
        hints,
    }
}
