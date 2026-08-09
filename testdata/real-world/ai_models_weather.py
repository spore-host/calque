# Fetched during calque#79 corpus-expansion pass.
# Origin: darothen/ai-models-for-all, files ai-models-modal/app.py + ai-models-modal/main.py
# https://github.com/darothen/ai-models-for-all/blob/main/ai-models-modal/app.py
# https://github.com/darothen/ai-models-for-all/blob/main/ai-models-modal/main.py
#
# Verbatim real-world source, merged into one file (the original is split
# across a Python package's app.py/main.py/config.py/gcs.py/gfs.py, joined
# by relative imports; only this header block and the merge itself are
# changes -- everything else is unmodified). A real GPU weather-forecast
# batch pipeline (same shape category as AI-Almanac's forecasts_app.py from
# calque#79) built entirely on plain @stub.function + one @stub.cls with a
# custom __init__/__enter__ (NOT @modal.enter) -- Modal's pre-`modal.parameter()`
# lifecycle-class pattern. Also exercises: `modal.NetworkFileSystem.persisted`,
# `modal.is_local()`, `allow_cross_region_volumes=True`, `concurrency_limit=1`
# (old-era autoscaling kwarg on a @cls, not just @function), and a bare
# `.local(...)` call from a plain function into another plain function
# (`make_model_era5_template.local(model_name)`).

"""Modal object definitions for reference by other application components."""
import os

import modal

from . import ai_models_shim, config

logger = config.get_logger(__name__)


def download_model_assets():
    """Download and cache the model weights necessary to run the model."""
    raise Exception(
        "This function is deprecated; assets will be downloaded on the first run of a model"
        " and saved to an NFS running within the application."
    )

    from ai_models import model
    from multiurl import download

    # For each model, retrieve the pretrained model weights and cache them to
    # our volume. We are generally replicating the code from
    # ai_models.model.Model.download_assets(), but with some hard-coded options;
    # that method is also originally written as an instance method, and we don't
    # want to run the actual initializer for a model type to access it since
    # that would require us to provide input/output options and otherwise
    # prepare more generally for a model inference run - something we're not
    # ready to do at this stage of setup.
    n_models = len(config.SUPPORTED_AI_MODELS)
    for i, model_name in enumerate(config.SUPPORTED_AI_MODELS, 1):
        logger.info(f"({i}/{n_models}) downloading assets for model {model_name}...")
        model_initializer = model.available_models()[model_name].load()
        for file in model_initializer.download_files:
            asset = os.path.realpath(os.path.join(config.AI_MODEL_ASSETS_DIR, file))
            if not os.path.exists(asset):
                os.makedirs(os.path.dirname(asset), exist_ok=True)
                logger.info("downloading %s", asset)
                download(
                    model_initializer.download_url.format(file=file),
                    asset + ".download",
                )
                os.rename(asset + ".download", asset)


# Set up the image that we'll use for performing model inference.
# NOTE: We use a somewhat convoluted build procedure here, but after much trial
# and error, this seems to reliably build a working application. The biggest
# issue we ran into was getting onnx to detect our GPU and NVIDIA libraries
# correctly. To achieve this, we manually install via mamba a known, working
# combination of CUDA and cuDNN. We also have to be careful when we install the
# library for model-specific plugins to ai-models; these tended to re-install
# the CPU-only onnxruntime library, so we manually uninstall that and purposely
# install the onnxrtuntime-gpu library instead.
# TODO: Explore whether we can consolidate the outputs from all of these pip
# installation steps into a single, master requirements.txt. Several packages seem
# to produce redundant requirements that lead to some version ping-ponging during
# setup. A deterministic process to produce a single requirements set that we could
# keep up-to-date would improve maintainability.
inference_image = (
    modal.Image
    # Micromamba will be much faster than conda, but we need to pin to
    # Python=3.10 to ensure ai-models' dependencies work correctly.
    .micromamba(python_version="3.10")
    .apt_install(
        [
            "git",
        ]
    )
    .micromamba_install(
        "cudatoolkit=11.8",
        "cudnn<=8.7.0",
        "eccodes",
        "pygrib",
        channels=[
            "conda-forge",
        ],
    )
    # Run several successive pip installs; this makes it a little bit easier to
    # handle the dependencies and final tweaks across different plugins.
    # (1) Install ai-models and its dependencies.
    .pip_install(
        [
            "ai-models",
            "google-cloud-storage",
            "onnx==1.15.0",
            "ujson",
        ]
    )
    # (2) GraphCast has some additional requirements - mostly around building a
    # properly configured version of JAX that can run on GPUs - so we take care
    # of those here.
    .pip_install(
        ["jax[cuda11_pip]==0.4.20", "git+https://github.com/deepmind/graphcast.git"],
        find_links="https://storage.googleapis.com/jax-releases/jax_cuda_releases.html",
    )
    # (3) Install the ai-models plugins enabled for this package.
    .pip_install(
        [
            "ai-models-" + plugin_config.plugin_package_name
            for plugin_config in ai_models_shim.AI_MODELS_CONFIGS.values()
        ]
    )
    .run_commands("pip uninstall -y onnxruntime")
    # (4) Ensure that we're using the ONNX GPU-enabled runtime.
    .pip_install("onnxruntime-gpu==1.16.3")
    # Generate a blank .cdsapirc file so that we can override credentials with
    # environment variables later on. This is necessary because the ai-models
    # package input handler ultimately uses climetlab.sources.CDSAPIKeyPrompt to
    # create a client to the CDS API, and it has a hard-coded prompt check
    # which requires user interaction if this file doesn't exist.
    # TODO: Patch climetlab to allow env var overrides for CDS API credentials.
    .run_commands("touch /root/.cdsapirc")
)

# Set up a storage volume for sharing model outputs between processes.
# TODO: Explore adding a modal.Volume to cache model weights since it should be
# much faster for loading them at runtime.
volume = modal.NetworkFileSystem.persisted("ai-models-cache")

stub = modal.Stub(name="ai-models-for-all", image=inference_image)


# --- everything below is main.py, merged; imports of `stub`/`volume` from
# the sibling app.py module inlined since they're defined above already ---

"""A Modal application for running `ai-models` weather forecasts."""

import datetime
import pathlib
import shutil

from ai_models import model
from tqdm import tqdm
from tqdm.contrib.logging import logging_redirect_tqdm

from . import gcs

config.set_logger_basic_config()


@stub.function(
    image=stub.image,
    secrets=[config.ENV_SECRETS],
    network_file_systems={str(config.CACHE_DIR): volume},
    timeout=300,
)
def prepare_gfs_analysis(
    model_name: str = "panguweather",
    model_init: datetime.datetime = datetime.datetime(2023, 7, 1, 0, 0),
    force: bool = config.FORCE_OVERRIDE,
):
    """Retrieve and prepare initial conditions from the GFS/GDAS to run with an AI model."""
    from . import gfs

    logger.info(f"Preparing GFS/GDAS initial conditions for {model_name} model run...")

    template_pth = config.make_gfs_template_path(model_name)
    if not template_pth.exists():
        raise ValueError(
            f"Expected to find GFS/GDAS -> ERA-5 template at {template_pth}, but file does not exist."
        )

    gdas_base_pth = gfs.make_gfs_base_pth(model_init)
    gdas_base_pth.mkdir(parents=True, exist_ok=True)

    proc_gdas_fn = f"gdas.proc-{model_name}.grib"
    final_proc_gdas_pth = gdas_base_pth / proc_gdas_fn

    if final_proc_gdas_pth.exists() and not force:
        logger.info(
            f"Found existing processed GFS/GDAS file {gdas_base_pth / proc_gdas_fn};"
            " skipping download and processing."
        )
        return

    service_account_info = gcs.get_service_account_json("GCS_SERVICE_ACCOUNT_INFO")
    gcs_handler = gcs.GoogleCloudStorageHandler.with_service_account_info(
        service_account_info
    )

    match model_name:
        case "panguweather" | "fourcastnetv2-small":
            model_init_tds = [
                datetime.timedelta(hours=0),
            ]
            source_blob_names = [
                gfs.make_gfs_ics_blob_name(model_init + td) for td in model_init_tds
            ]
        case "graphcast":
            model_init_tds = [
                datetime.timedelta(hours=0),
                datetime.timedelta(hours=-6),
            ]
            source_blob_names = [
                gfs.make_gfs_ics_blob_name(model_init + td) for td in model_init_tds
            ]
        case _:
            raise ValueError(f"Encountered unknown model {model_name}")

    source_fns = [blob_name.split("/")[-1] for blob_name in source_blob_names]
    for source_blob_name, source_fn in zip(source_blob_names, source_fns):
        gcs_handler.download_blob(gfs.GFS_BUCKET, source_blob_name, source_fn)
        if not pathlib.Path(source_fn).exists():
            raise RuntimeError("Failed to download GFS/GDAS blob.")

    subset_grbs = gfs.process_gdas_grib(template_pth, source_fns[0], model_init)

    with open(proc_gdas_fn, "wb") as f:
        for grb in tqdm(subset_grbs, unit="msg", total=len(subset_grbs)):
            f.write(grb.tostring())
    shutil.copy(proc_gdas_fn, final_proc_gdas_pth)


@stub.cls(
    secrets=[config.ENV_SECRETS],
    gpu=config.DEFAULT_GPU_CONFIG,
    network_file_systems={str(config.CACHE_DIR): volume},
    concurrency_limit=1,
    timeout=1_800,
)
class AIModel:
    def __init__(
        self,
        model_name: str = "panguweather",
        model_init: datetime.datetime = datetime.datetime(2023, 7, 1, 0, 0),
        lead_time: int = 12,
        use_gfs: bool = False,
    ) -> None:
        self.model_name = model_name
        self.model_init = model_init

        if lead_time > config.MAX_FCST_LEAD_TIME:
            self.lead_time = config.MAX_FCST_LEAD_TIME
        else:
            self.lead_time = lead_time

        self.out_pth = config.make_output_path(model_name, model_init, use_gfs)
        self.out_pth.parent.mkdir(parents=True, exist_ok=True)

        self.use_gfs = use_gfs

    def __enter__(self):
        logger.info(f"   Model: {self.model_name}")
        if self.use_gfs:
            self.init_model = self._init_model_for_gfs()
        else:
            self.init_model = self._init_model_for_era5()

    def _init_model_for_era5(self):
        model_class = ai_models_shim.get_model_class(self.model_name)
        return model_class(
            input="cds",
            output="file",
            download_assets=False,
            assets=config.AI_MODEL_ASSETS_DIR,
            date=int(self.model_init.strftime("%Y%m%d")),
            time=self.model_init.hour,
            lead_time=self.lead_time,
            path=str(self.out_pth),
            metadata={},
            model_args={},
            assets_sub_directory=None,
            staging_dates=None,
            archive_requests=False,
            only_gpu=True,
            debug=False,
        )

    def _init_model_for_gfs(self):
        from . import gfs

        model_class = ai_models_shim.get_model_class(self.model_name)

        gdas_base_pth = gfs.make_gfs_base_pth(self.model_init)
        gdas_proc_fn = f"gdas.proc-{self.model_name}.grib"
        gdas_proc_pth = gdas_base_pth / gdas_proc_fn
        if not gdas_proc_pth.exists():
            raise RuntimeError(
                f"Expected processed GFS/GDAS initial conditions file not found at"
                f" {gdas_proc_fn}."
            )
        shutil.copy(gdas_proc_pth, gdas_proc_fn)

        return model_class(
            output="file",
            download_assets=False,
            assets=config.AI_MODEL_ASSETS_DIR,
            date=int(self.model_init.strftime("%Y%m%d")),
            time=self.model_init.hour,
            lead_time=self.lead_time,
            path=str(self.out_pth),
            metadata={},
            model_args={},
            assets_sub_directory=None,
            staging_dates=None,
            archive_requests=False,
            only_gpu=True,
            debug=False,
            input="file",
            file=str(gdas_proc_fn),
        )

    @modal.method()
    def run_model(self) -> None:
        logger.info("Invoking AIModel.run_model()...")
        self.init_model.run()


def _maybe_download_assets(model_name: str) -> None:
    from multiurl import download

    model_class = ai_models_shim.get_model_class(model_name)
    n_downloaded = 0
    for file in model_class.download_files:
        asset = os.path.realpath(os.path.join(config.AI_MODEL_ASSETS_DIR, file))
        if not os.path.exists(asset):
            os.makedirs(os.path.dirname(asset), exist_ok=True)
            download(
                model_class.download_url.format(file=file),
                asset + ".download",
            )
            os.rename(asset + ".download", asset)
            n_downloaded += 1

    template_pth = config.make_gfs_template_path(model_name)
    if not template_pth.exists():
        bucket_name = os.environ.get("GCS_BUCKET_NAME", "")
        service_account_info = gcs.get_service_account_json("GCS_SERVICE_ACCOUNT_INFO")
        gcs_handler = gcs.GoogleCloudStorageHandler.with_service_account_info(
            service_account_info
        )
        template_fn = template_pth.name
        target_blob = gcs_handler.client.bucket(bucket_name).blob(template_fn)

        if not target_blob.exists():
            # A plain function calling another plain function via .local() --
            # in-container, no new invocation, same shape as calque#92's
            # local_chain.py fixture but found independently in real code.
            make_model_era5_template.local(model_name)

        gcs_handler.download_blob(bucket_name, template_fn, template_pth)


@stub.function(
    image=stub.image,
    secrets=[config.ENV_SECRETS],
    network_file_systems={str(config.CACHE_DIR): volume},
    allow_cross_region_volumes=True,
    timeout=1_800,
)
def generate_forecast(
    model_name: str = "panguweather",
    model_init: datetime.datetime = datetime.datetime(2023, 7, 1, 0, 0),
    lead_time: int = 12,
    use_gfs: bool = False,
    skip_validate_env: bool = False,
):
    """Generate a forecast using the specified model."""
    if not skip_validate_env:
        config.validate_env()

    _maybe_download_assets(model_name)
    if use_gfs:
        prepare_gfs_analysis.remote(model_name, model_init)
    ai_model = AIModel(model_name, model_init, lead_time, use_gfs)

    ai_model.run_model.remote()

    bucket_name = os.environ.get("GCS_BUCKET_NAME", "")
    service_account_info = gcs.get_service_account_json("GCS_SERVICE_ACCOUNT_INFO")

    if (bucket_name is None) or (not service_account_info):
        logger.warning("Not able to access to Google Cloud Storage; skipping upload.")
        return

    gcs_handler = gcs.GoogleCloudStorageHandler.with_service_account_info(
        service_account_info
    )
    dest_blob_name = ai_model.out_pth.name
    gcs_handler.upload_blob(
        bucket_name,
        ai_model.out_pth,
        dest_blob_name,
    )


@stub.function(
    image=stub.image,
    secrets=[config.ENV_SECRETS],
    network_file_systems={str(config.CACHE_DIR): volume},
    timeout=7_200,
    allow_cross_region_volumes=True,
)
def make_model_era5_template(model_name: str):
    """Generate a template GRIB file corresponding to the ERA-5 inputs for a given
    AI model."""
    import climetlab as cml
    import numpy as np

    bucket_name = os.environ.get("GCS_BUCKET_NAME", "")
    service_account_info = gcs.get_service_account_json("GCS_SERVICE_ACCOUNT_INFO")
    gcs_handler = gcs.GoogleCloudStorageHandler.with_service_account_info(
        service_account_info
    )

    model_class = ai_models_shim.get_model_class(model_name)
    model = model_class(  # noqa: F811
        input="cds",
        output="file",
        download_assets=False,
        assets=config.AI_MODEL_ASSETS_DIR,
        date=int(config.DEFAULT_GFS_TEMPLATE_MODEL_EPOCH.strftime("%Y%m%d")),
        time=int(config.DEFAULT_GFS_TEMPLATE_MODEL_EPOCH.strftime("%H")),
        lead_time=6,
        path="_stub.grib2",
        metadata={},
        model_args={},
        assets_sub_directory=None,
        staging_dates=None,
        archive_requests=False,
        only_gpu=False,
        debug=True,
    )

    out_fn = f"{model_name}.input-template.grib2"
    with cml.new_grib_output(out_fn) as f:
        for template in model.input.all_fields:
            f.write(np.zeros_like(template.shape), template=template)

    gcs_handler.upload_blob(
        bucket_name,
        out_fn,
        out_fn,
    )


@stub.local_entrypoint()
def main(
    model_name: str = "panguweather",
    lead_time: int = 12,
    model_init: datetime.datetime = datetime.datetime(2023, 7, 1, 0, 0),
    use_gfs: bool = False,
    make_template: bool = False,
    run_checks: bool = False,
    run_forecast: bool = False,
):
    """Entrypoint for triggering a remote ai-models weather forecast run."""
    if model_name not in ai_models_shim.SUPPORTED_AI_MODELS:
        raise ValueError(
            f"User provided model_name '{model_name}' is not supported; must be one of"
            f" {ai_models_shim.SUPPORTED_AI_MODELS}."
        )

    if make_template:
        make_model_era5_template.remote(model_name)
    if run_checks:
        logger.info(f"Running locally -> {modal.is_local()}")
    if run_forecast:
        generate_forecast.remote(
            model_name=model_name,
            model_init=model_init,
            lead_time=lead_time,
            use_gfs=use_gfs,
        )
